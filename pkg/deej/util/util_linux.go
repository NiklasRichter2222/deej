package util

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus"
)

const (
	getCurrentWindowInternalCooldown = time.Millisecond * 150
	kwinRetryCooldown                = time.Second * 5

	kwinBusName          = "org.kde.KWin"
	kwinScriptingPath    = "/Scripting"
	kwinScriptingIface   = "org.kde.kwin.Scripting"
	kwinScriptIface      = "org.kde.kwin.Script"
	kwinPluginName       = "deej_window_tracker"
	trackerObjectPath    = "/WindowTracker"
	trackerInterfaceName = "org.deej.WindowTracker"
)

type activeWindowInfo struct {
	pid           int
	resourceName  string
	resourceClass string
	desktopFile   string
}

var (
	getCurrentWindowMutex      sync.Mutex
	lastGetCurrentWindowResult []string
	lastGetCurrentWindowCall   time.Time
	lastResolvedWindow         activeWindowInfo

	kwinStateMu         sync.RWMutex
	currentActiveWindow activeWindowInfo
	kwinTrackerReady    bool
	lastKwinInitAttempt time.Time
	kwinDBusConn        *dbus.Conn
	kwinReceiverOnce    sync.Once
)

type kwinDBusReceiver struct{}

// NotifyActiveWindow is invoked over D-Bus by our KWin script whenever the active window changes.
func (r *kwinDBusReceiver) NotifyActiveWindow(pidStr, resourceName, resourceClass, desktopFile string) *dbus.Error {
	pid, _ := strconv.Atoi(strings.TrimSpace(pidStr))
	rn := strings.TrimSpace(resourceName)
	rc := strings.TrimSpace(resourceClass)
	df := strings.TrimSpace(desktopFile)

	// Ignore empty or helper XWayland/Wine focus-proxy windows that lack window class/name metadata
	if (pid <= 0 && rn == "" && rc == "") || (rn == "" && rc == "") {
		return nil
	}

	nextWindow := activeWindowInfo{
		pid:           pid,
		resourceName:  rn,
		resourceClass: rc,
		desktopFile:   df,
	}

	kwinStateMu.Lock()
	changed := currentActiveWindow != nextWindow
	currentActiveWindow = nextWindow
	kwinTrackerReady = true
	kwinStateMu.Unlock()

	if changed {
		// Invalidate cooldown cache so immediate slider actions reflect the newly focused window
		getCurrentWindowMutex.Lock()
		lastGetCurrentWindowCall = time.Time{}
		getCurrentWindowMutex.Unlock()

		scheduleActiveWindowNotify()
	}

	return nil
}

const activeWindowNotifyDebounce = 200 * time.Millisecond

var (
	activeWindowNotifyMu    sync.Mutex
	activeWindowNotifyTimer *time.Timer
)

// scheduleActiveWindowNotify coalesces rapid focus changes into a single callback once focus settles.
func scheduleActiveWindowNotify() {
	activeWindowNotifyMu.Lock()
	defer activeWindowNotifyMu.Unlock()

	if activeWindowNotifyTimer != nil {
		activeWindowNotifyTimer.Stop()
	}
	activeWindowNotifyTimer = time.AfterFunc(activeWindowNotifyDebounce, notifyActiveWindowChanged)
}

func getCurrentWindowProcessNames() ([]string, error) {
	getCurrentWindowMutex.Lock()
	defer getCurrentWindowMutex.Unlock()

	winInfo, err := getActiveWindowInfo()
	if err != nil {
		return nil, err
	}

	if winInfo.pid <= 0 && winInfo.resourceName == "" && winInfo.resourceClass == "" {
		lastGetCurrentWindowResult = nil
		lastResolvedWindow = activeWindowInfo{}
		return nil, nil
	}

	now := time.Now()
	if winInfo == lastResolvedWindow && len(lastGetCurrentWindowResult) > 0 {
		return lastGetCurrentWindowResult, nil
	}
	if !lastGetCurrentWindowCall.IsZero() && lastGetCurrentWindowCall.Add(getCurrentWindowInternalCooldown).After(now) && winInfo == lastResolvedWindow {
		return lastGetCurrentWindowResult, nil
	}
	lastGetCurrentWindowCall = now

	names := resolveProcessNamesForWindow(winInfo)
	lastResolvedWindow = winInfo
	lastGetCurrentWindowResult = names
	return lastGetCurrentWindowResult, nil
}

func getActiveWindowInfo() (activeWindowInfo, error) {
	ensureKWinTracker()

	kwinStateMu.RLock()
	ready := kwinTrackerReady
	info := currentActiveWindow
	kwinStateMu.RUnlock()

	if ready {
		return info, nil
	}

	// Fallback for non-KWin environments (kdotool / xdotool)
	pidStr, err := getActiveWindowPIDFallback()
	if err != nil {
		return activeWindowInfo{}, err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
	if err != nil {
		return activeWindowInfo{}, fmt.Errorf("invalid pid: %s", pidStr)
	}

	return activeWindowInfo{pid: pid}, nil
}

func ensureKWinTracker() {
	kwinStateMu.RLock()
	ready := kwinTrackerReady
	lastAttempt := lastKwinInitAttempt
	kwinStateMu.RUnlock()

	if ready || (!lastAttempt.IsZero() && lastAttempt.Add(kwinRetryCooldown).After(time.Now())) {
		return
	}

	kwinStateMu.Lock()
	if kwinTrackerReady || (!lastKwinInitAttempt.IsZero() && lastKwinInitAttempt.Add(kwinRetryCooldown).After(time.Now())) {
		kwinStateMu.Unlock()
		return
	}
	lastKwinInitAttempt = time.Now()
	conn := kwinDBusConn
	kwinStateMu.Unlock()

	if !ensureDBusSessionEnv() {
		return
	}

	if conn == nil {
		var err error
		conn, err = dbus.SessionBus()
		if err != nil {
			return
		}

		kwinStateMu.Lock()
		kwinDBusConn = conn
		kwinStateMu.Unlock()

		kwinReceiverOnce.Do(func() {
			_ = conn.Export(&kwinDBusReceiver{}, dbus.ObjectPath(trackerObjectPath), trackerInterfaceName)
			watchKWinRestart(conn)
		})
	}

	_ = loadKWinTrackerScript(conn)
}

func ensureDBusSessionEnv() bool {
	if addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr != "" && addr != "autolaunch:" {
		return true
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	busPath := filepath.Join(runtimeDir, "bus")
	if _, err := os.Stat(busPath); err == nil {
		_ = os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+busPath)
		return true
	}
	return false
}

func loadKWinTrackerScript(conn *dbus.Conn) error {
	names := conn.Names()
	if len(names) == 0 {
		return fmt.Errorf("dbus connection has no bus name")
	}
	selfBusName := names[0]

	jsCode := fmt.Sprintf(`function reportWindow(w) {
    if (!w || w !== workspace.activeWindow || !w.active) {
        return;
    }
    if (w.specialWindow || (!w.normalWindow && !w.dialog)) {
        return;
    }
    var rn = String(w.resourceName || "");
    var rc = String(w.resourceClass || "");
    var lowerRn = rn.toLowerCase();
    var lowerRc = rc.toLowerCase();
    if ((rn === "" && rc === "") || lowerRn === "dimland" || lowerRc === "dimland" || lowerRn === "xembedsniproxy" || lowerRn === "plasmashell") {
        return;
    }
    callDBus(%q, %q, %q, "NotifyActiveWindow",
             String(w.pid || 0),
             rn,
             rc,
             String(w.desktopFileName || ""));
}
workspace.windowActivated.connect(function (w) { reportWindow(workspace.activeWindow); });
reportWindow(workspace.activeWindow);
`, selfBusName, trackerObjectPath, trackerInterfaceName)

	jsPath := filepath.Join(os.TempDir(), fmt.Sprintf("deej-kwin-tracker-%d.js", os.Getuid()))
	if err := ioutil.WriteFile(jsPath, []byte(jsCode), 0600); err != nil {
		return err
	}

	scriptingObj := conn.Object(kwinBusName, dbus.ObjectPath(kwinScriptingPath))

	// Unload any previous instance of our script
	_ = scriptingObj.Call(kwinScriptingIface+".unloadScript", 0, kwinPluginName).Err

	var scriptID int32
	if err := scriptingObj.Call(kwinScriptingIface+".loadScript", 0, jsPath, kwinPluginName).Store(&scriptID); err != nil {
		return err
	}

	// Plasma 6 uses /Scripting/Script<id>, Plasma 5 uses /<id>
	scriptPath := dbus.ObjectPath(fmt.Sprintf("/Scripting/Script%d", scriptID))
	if err := conn.Object(kwinBusName, scriptPath).Call(kwinScriptIface+".run", 0).Err; err != nil {
		fallbackPath := dbus.ObjectPath(fmt.Sprintf("/%d", scriptID))
		if err2 := conn.Object(kwinBusName, fallbackPath).Call(kwinScriptIface+".run", 0).Err; err2 != nil {
			return err
		}
		scriptPath = fallbackPath
	}

	kwinStateMu.Lock()
	kwinTrackerReady = true
	kwinStateMu.Unlock()

	return nil
}

// CleanupWindowTracker unloads the temporary KWin script and removes its temporary file on shutdown.
func CleanupWindowTracker() {
	kwinStateMu.Lock()
	conn := kwinDBusConn
	kwinTrackerReady = false
	kwinStateMu.Unlock()

	if conn != nil {
		scriptingObj := conn.Object(kwinBusName, dbus.ObjectPath(kwinScriptingPath))
		_ = scriptingObj.Call(kwinScriptingIface+".unloadScript", 0, kwinPluginName).Err
	}

	jsPath := filepath.Join(os.TempDir(), fmt.Sprintf("deej-kwin-tracker-%d.js", os.Getuid()))
	_ = os.Remove(jsPath)
}

func watchKWinRestart(conn *dbus.Conn) {
	rule := "type='signal',sender='org.freedesktop.DBus',interface='org.freedesktop.DBus',member='NameOwnerChanged',arg0='" + kwinBusName + "'"
	_ = conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule).Err

	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)

	go func() {
		for sig := range sigCh {
			if sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) < 3 {
				continue
			}
			name, _ := sig.Body[0].(string)
			newOwner, _ := sig.Body[2].(string)
			if name == kwinBusName && newOwner != "" {
				kwinStateMu.Lock()
				kwinTrackerReady = false
				lastKwinInitAttempt = time.Time{}
				kwinStateMu.Unlock()

				_ = loadKWinTrackerScript(conn)
			}
		}
	}()
}

func getActiveWindowPIDFallback() (string, error) {
	out, err := exec.Command("kdotool", "getactivewindow", "getwindowpid").Output()
	if err == nil {
		return string(out), nil
	}

	out, err = exec.Command("xdotool", "getactivewindow", "getwindowpid").Output()
	if err == nil {
		return string(out), nil
	}

	return "", fmt.Errorf("could not get active window pid via KWin D-Bus, kdotool, or xdotool")
}

func resolveProcessNamesForWindow(info activeWindowInfo) []string {
	seen := make(map[string]struct{})
	var results []string

	addCandidate := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		lower := strings.ToLower(name)
		if _, exists := seen[lower]; !exists {
			seen[lower] = struct{}{}
			results = append(results, name)
		}
	}

	if info.pid > 0 {
		primaryNames := getProcNamesForPID(info.pid)
		isWineApp := false
		for _, n := range primaryNames {
			if strings.HasSuffix(strings.ToLower(n), ".exe") {
				isWineApp = true
			}
			addCandidate(n)
		}

		// For Wine/Proton .exe processes, the window PID is already the specific .exe process;
		// avoid scanning /proc descendants/parents so switching between Wine and native apps is instantaneous.
		if !isWineApp {
			for _, childPID := range getDescendantPIDs(info.pid) {
				for _, n := range getProcNamesForPID(childPID) {
					addCandidate(n)
				}
			}

			if ppid := getParentPID(info.pid); ppid > 1 {
				parentNames := getProcNamesForPID(ppid)
				if len(parentNames) > 0 && !isSystemSupervisor(parentNames[0]) {
					for _, n := range parentNames {
						addCandidate(n)
					}
				}
			}
		}
	}

	addCandidate(info.resourceName)
	addCandidate(info.resourceClass)

	if info.desktopFile != "" {
		base := filepath.Base(info.desktopFile)
		base = strings.TrimSuffix(base, ".desktop")
		addCandidate(base)
		if idx := strings.LastIndex(base, "."); idx != -1 && idx+1 < len(base) {
			addCandidate(base[idx+1:])
		}
	}

	return results
}

func getProcNamesForPID(pid int) []string {
	var names []string
	procDir := fmt.Sprintf("/proc/%d", pid)

	var exeBase string
	if exePath, err := os.Readlink(filepath.Join(procDir, "exe")); err == nil && exePath != "" {
		exePath = strings.TrimSuffix(exePath, " (deleted)")
		base := filepath.Base(exePath)
		if base != "" && base != "." && base != "/" {
			exeBase = base
		}
	}

	// If this process is a Wine/Proton loader, prefer its specific Windows .exe name over generic wine64-preloader
	if isWineLoaderBinary(exeBase) {
		if wineExe := extractWineExeFromPID(pid); wineExe != "" {
			names = append(names, wineExe)
		} else {
			names = append(names, exeBase)
		}
	} else if exeBase != "" {
		names = append(names, exeBase)
	}

	// 2. /proc/<pid>/cmdline argv[0] (and Wine .exe arguments if applicable)
	if cmdlineBytes, err := ioutil.ReadFile(filepath.Join(procDir, "cmdline")); err == nil && len(cmdlineBytes) > 0 {
		parts := bytes.Split(cmdlineBytes, []byte{0})
		for i, part := range parts {
			if len(part) == 0 {
				continue
			}
			arg := string(part)
			lowerArg := strings.ToLower(arg)
			if strings.HasSuffix(lowerArg, ".exe") {
				normalized := strings.ReplaceAll(arg, "\\", "/")
				base := filepath.Base(normalized)
				if base != "" && base != "." && base != "/" && !isWineSystemWrapperExe(base) {
					names = append(names, base)
				}
			} else if i == 0 {
				// Strip flags if argv[0] was overwritten with a command line string (e.g. Chromium utility processes)
				if exeIdx := strings.Index(lowerArg, ".exe "); exeIdx != -1 {
					arg = arg[:exeIdx+4]
				} else if spaceIdx := strings.IndexByte(arg, ' '); spaceIdx != -1 {
					arg = arg[:spaceIdx]
				}
				normalized := strings.ReplaceAll(arg, "\\", "/")
				base := filepath.Base(normalized)
				if base != "" && base != "." && base != "/" && !isWineLoaderBinary(base) {
					names = append(names, base)
				}
			}
		}
	}

	// 3. /proc/<pid>/comm (kernel comm name, up to 15 chars)
	if commBytes, err := ioutil.ReadFile(filepath.Join(procDir, "comm")); err == nil {
		comm := strings.TrimSpace(string(commBytes))
		if comm != "" && !isWineLoaderBinary(comm) {
			names = append(names, comm)
		}
	}

	return names
}

// ResolveProcessBinaryName returns the effective binary name for an audio session.
// For Wine/Proton processes (where PulseAudio reports wine64-preloader or wine-preloader),
// it inspects /proc/<pid>/cmdline to return the actual Windows .exe name (e.g. "ts3client_win64.exe" or "Arma3_x64.exe").
func ResolveProcessBinaryName(binaryName, pidStr string) string {
	binaryName = strings.TrimSpace(binaryName)
	if isWineLoaderBinary(binaryName) && pidStr != "" {
		if pid, err := strconv.Atoi(strings.TrimSpace(pidStr)); err == nil && pid > 0 {
			if exeName := extractWineExeFromPID(pid); exeName != "" {
				return exeName
			}
		}
	}
	return binaryName
}

func isWineLoaderBinary(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "wine64-preloader", "wine-preloader", "wine", "wine64":
		return true
	default:
		return false
	}
}

func isWineSystemWrapperExe(exeBase string) bool {
	switch strings.ToLower(strings.TrimSpace(exeBase)) {
	case "steam.exe", "start.exe", "wineboot.exe", "services.exe", "winedevice.exe",
		"plugplay.exe", "svchost.exe", "rpcss.exe", "explorer.exe", "conhost.exe",
		"tabtip.exe", "xalia.exe":
		return true
	default:
		return false
	}
}

func extractWineExeFromPID(pid int) string {
	cmdlineBytes, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(cmdlineBytes) == 0 {
		return ""
	}

	var fallbackExe string
	parts := bytes.Split(cmdlineBytes, []byte{0})
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		arg := strings.TrimSpace(string(part))
		lowerArg := strings.ToLower(arg)

		// Handle either a pure .exe argument or a command string containing ".exe "
		if !strings.HasSuffix(lowerArg, ".exe") {
			if exeIdx := strings.Index(lowerArg, ".exe "); exeIdx != -1 {
				arg = arg[:exeIdx+4]
			} else {
				continue
			}
		}

		normalized := strings.ReplaceAll(arg, "\\", "/")
		base := filepath.Base(normalized)
		if base == "" || base == "." || base == "/" {
			continue
		}

		if !isWineSystemWrapperExe(base) {
			return base
		}
		if fallbackExe == "" {
			fallbackExe = base
		}
	}

	return fallbackExe
}

func getParentPID(pid int) int {
	data, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	closeParen := strings.LastIndexByte(s, ')')
	if closeParen == -1 || closeParen+2 >= len(s) {
		return 0
	}
	var state rune
	var ppid int
	if _, err := fmt.Sscanf(s[closeParen+2:], "%c %d", &state, &ppid); err != nil {
		return 0
	}
	return ppid
}

func getDescendantPIDs(rootPID int) []int {
	entries, err := ioutil.ReadDir("/proc")
	if err != nil {
		return nil
	}

	childrenMap := make(map[int][]int)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 {
			continue
		}
		ppid := getParentPID(pid)
		if ppid > 1 {
			childrenMap[ppid] = append(childrenMap[ppid], pid)
		}
	}

	var descendants []int
	queue := append([]int(nil), childrenMap[rootPID]...)
	visited := map[int]bool{rootPID: true}

	for len(queue) > 0 && len(descendants) < 64 {
		curr := queue[0]
		queue = queue[1:]
		if visited[curr] {
			continue
		}
		visited[curr] = true
		descendants = append(descendants, curr)
		queue = append(queue, childrenMap[curr]...)
	}

	return descendants
}

func isSystemSupervisor(name string) bool {
	switch strings.ToLower(name) {
	case "systemd", "init", "kwin_wayland", "kwin_x11", "plasmashell", "bash", "zsh", "fish", "sh", "dbus-daemon",
		"pv-adverb", "srt-bwrap", "reaper", "steam", "wineserver":
		return true
	default:
		return false
	}
}
