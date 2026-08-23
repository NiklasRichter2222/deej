package deej

import (
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	ole "github.com/go-ole/go-ole"
	"github.com/omriharel/deej/pkg/deej/util"
	"github.com/thoas/go-funk"
	"go.uber.org/zap"
)

type sessionMap struct {
	deej   *Deej
	logger *zap.SugaredLogger

	m    map[string][]Session
	lock sync.RWMutex

	sessionFinder SessionFinder

	lastSessionRefresh time.Time
	unmappedSessions   []Session

	sliderSyncStop     chan struct{}
	sliderSyncStopOnce sync.Once
}

const (
	masterSessionName = "master" // master device volume
	systemSessionName = "system" // system sounds volume
	inputSessionName  = "mic"    // microphone input level

	// some targets need to be transformed before their correct audio sessions can be accessed.
	// this prefix identifies those targets to ensure they don't contradict with another similarly-named process
	specialTargetTransformPrefix = "deej."

	// targets the currently active window (Windows-only, experimental)
	specialTargetCurrentWindow = "current"

	// targets all currently unmapped sessions (experimental)
	specialTargetAllUnmapped = "unmapped"

	// this threshold constant assumes that re-acquiring all sessions is a kind of expensive operation,
	// and needs to be limited in some manner. this value was previously user-configurable through a config
	minTimeBetweenSessionRefreshes = time.Second * 1

	// smallest interval between forced refreshes when the current window target is in play
	currentTargetForceRefreshCooldown = time.Millisecond * 500

	// smallest interval between forced refreshes for fixed (non-current) targets
	missingTargetForceRefreshCooldown = time.Millisecond * 900

	// determines whether the map should be refreshed when a slider moves.
	// this is a bit greedy but allows us to ensure sessions are always re-acquired, which is
	// especially important for process groups (because you can have one ongoing session
	// always preventing lookup of other processes bound to its slider, which forces the user
	// to manually refresh sessions). a cleaner way to do this down the line is by registering to notifications
	// whenever a new session is added, but that's too hard to justify for how easy this solution is
	maxTimeBetweenSessionRefreshes = time.Second * 45
)

// this matches friendly device names (on Windows), e.g. "Headphones (Realtek Audio)"
var deviceSessionKeyPattern = regexp.MustCompile(`^.+ \(.+\)$`)

func newSessionMap(deej *Deej, logger *zap.SugaredLogger, sessionFinder SessionFinder) (*sessionMap, error) {
	logger = logger.Named("sessions")

	m := &sessionMap{
		deej:           deej,
		logger:         logger,
		m:              make(map[string][]Session),
		sessionFinder:  sessionFinder,
		sliderSyncStop: make(chan struct{}),
	}

	logger.Debug("Created session map instance")

	return m, nil
}

func (m *sessionMap) initialize() error {
	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to get all sessions during session map initialization", "error", err)
		return fmt.Errorf("get all sessions during init: %w", err)
	}

	m.setupOnConfigReload()
	m.setupOnSliderMove()
	m.setupSliderVolumeSync()

	return nil
}

func (m *sessionMap) release() error {
	m.sliderSyncStopOnce.Do(func() {
		close(m.sliderSyncStop)
	})

	m.clear()

	if err := m.sessionFinder.Release(); err != nil {
		m.logger.Warnw("Failed to release session finder during session map release", "error", err)
		return fmt.Errorf("release session finder during release: %w", err)
	}

	return nil
}

// getAndAddSessions acquires all current sessions from SessionFinder, atomically swaps the map, and safely releases old sessions.
func (m *sessionMap) getAndAddSessions() error {
	sessions, err := m.sessionFinder.GetAllSessions()
	if err != nil {
		m.logger.Warnw("Failed to get sessions from session finder", "error", err)
		return fmt.Errorf("get sessions from SessionFinder: %w", err)
	}

	newMap := make(map[string][]Session)
	var newUnmapped []Session

	for _, session := range sessions {
		key := session.Key()
		newMap[key] = append(newMap[key], session)

		if !m.sessionMapped(session) {
			m.logger.Debugw("Tracking unmapped session", "session", session)
			newUnmapped = append(newUnmapped, session)
		}
	}

	// Atomically swap the session map under exclusive lock and capture old sessions for release
	m.lock.Lock()
	var oldSessions []Session
	for _, oldList := range m.m {
		oldSessions = append(oldSessions, oldList...)
	}
	m.m = newMap
	m.unmappedSessions = newUnmapped
	m.lastSessionRefresh = time.Now()
	m.lock.Unlock()

	// Safely release the old sessions now that no ongoing readers can access them
	for _, oldSession := range oldSessions {
		oldSession.Release()
	}

	m.logger.Infow("Got all audio sessions successfully", "sessionMap", m)

	if m.deej.config.SyncVolumes {
		m.syncAllSliderVolumes()
	}

	return nil
}

func (m *sessionMap) setupOnConfigReload() {
	configReloadedChannel := m.deej.config.SubscribeToChanges()

	go func() {
		runtime.LockOSThread()
		_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
		for {
			select {
			case <-configReloadedChannel:
				m.logger.Info("Detected config reload, attempting to re-acquire all audio sessions")
				m.refreshSessions(false)
			}
		}
	}()
}

func (m *sessionMap) setupOnSliderMove() {
	sliderEventsChannel := m.deej.serial.SubscribeToSliderMoveEvents()

	go func() {
		runtime.LockOSThread()
		_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
		for {
			select {
			case event := <-sliderEventsChannel:
				m.handleSliderMoveEvent(event)
			}
		}
	}()
}

// performance: explain why force == true at every such use to avoid unintended forced refresh spams
func (m *sessionMap) refreshSessions(force bool) {
	// make sure enough time passed since the last refresh, unless force is true in which case always clear
	if !force && m.lastSessionRefresh.Add(minTimeBetweenSessionRefreshes).After(time.Now()) {
		return
	}

	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to re-acquire all audio sessions", "error", err)
	} else {
		m.logger.Debug("Re-acquired sessions successfully")
	}
}

// returns true if a session is not currently mapped to any slider, false otherwise
// special sessions (master, system, mic) and device-specific sessions always count as mapped,
// even when absent from the config. this makes sense for every current feature that uses "unmapped sessions"
func (m *sessionMap) sessionMapped(session Session) bool {

	// count master/system/mic as mapped
	if funk.ContainsString([]string{masterSessionName, systemSessionName, inputSessionName}, session.Key()) {
		return true
	}

	// count device sessions as mapped
	if deviceSessionKeyPattern.MatchString(session.Key()) {
		return true
	}

	matchFound := false

	// look through the actual mappings
	m.deej.config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		for _, target := range targets {

			// ignore special transforms
			if m.targetHasSpecialTransform(target) {
				continue
			}

			// safe to assume this has a single element because we made sure there's no special transform
			target = m.resolveTarget(target)[0]

			if target == session.Key() {
				matchFound = true
				return
			}
		}
	})

	return matchFound
}

func (m *sessionMap) handleSliderMoveEvent(event SliderMoveEvent) {

	// first of all, ensure our session map isn't moldy
	if m.lastSessionRefresh.Add(maxTimeBetweenSessionRefreshes).Before(time.Now()) {
		m.logger.Debug("Stale session map detected on slider move, refreshing")
		m.refreshSessions(true)
	}

	// get the targets mapped to this slider from the config
	targets, ok := m.deej.config.SliderMapping.get(event.SliderID)

	// if slider not found in config, silently ignore
	if !ok {
		return
	}

	targetFound := false
	adjustmentFailed := false
	containsCurrentTarget := false

	// acquire read lock while adjusting volumes of matching sessions
	m.lock.RLock()
	for _, target := range targets {
		trimmedTarget := strings.ToLower(strings.TrimSpace(target))
		if trimmedTarget == specialTargetTransformPrefix+specialTargetCurrentWindow {
			containsCurrentTarget = true
		}

		resolvedTargets := m.resolveTarget(target)

		for _, resolvedTarget := range resolvedTargets {
			sessions, ok := m.m[resolvedTarget]
			if !ok || len(sessions) == 0 {
				continue
			}

			targetFound = true

			for _, session := range sessions {
				if session.GetVolume() != event.PercentValue {
					if err := session.SetVolume(event.PercentValue); err != nil {
						m.logger.Warnw("Failed to set target session volume", "error", err)
						adjustmentFailed = true
					}
				}
			}
		}
	}
	m.lock.RUnlock()

	// if we still haven't found a target or the volume adjustment failed, look for the target again.
	// processes could've opened since the last time this slider moved.
	if !targetFound {
		elapsed := time.Since(m.lastSessionRefresh)
		if containsCurrentTarget && elapsed > currentTargetForceRefreshCooldown {
			m.refreshSessions(true)
		} else if elapsed > missingTargetForceRefreshCooldown {
			m.refreshSessions(true)
		} else {
			m.refreshSessions(false)
		}
	} else if adjustmentFailed {
		m.refreshSessions(true)
	}
}

// sliderVolume reports the current average volume for all sessions mapped to a slider.
func (m *sessionMap) sliderVolume(sliderIdx int) (float32, bool) {
	targets, ok := m.deej.config.SliderMapping.get(sliderIdx)
	if !ok || len(targets) == 0 {
		return 0, false
	}

	preferredVolumes := make([]float32, 0)
	fallbackVolumes := make([]float32, 0)
	preferCurrent := false

	m.lock.RLock()
	defer m.lock.RUnlock()

	for _, target := range targets {
		trimmedTarget := strings.ToLower(strings.TrimSpace(target))
		resolvedTargets := m.resolveTarget(target)

		for _, resolvedTarget := range resolvedTargets {
			sessions, ok := m.m[resolvedTarget]
			if !ok || len(sessions) == 0 {
				continue
			}

			for _, session := range sessions {
				volume := session.GetVolume()
				if trimmedTarget == specialTargetTransformPrefix+specialTargetCurrentWindow {
					preferCurrent = true
					preferredVolumes = append(preferredVolumes, volume)
				} else {
					fallbackVolumes = append(fallbackVolumes, volume)
				}
			}
		}
	}

	volumes := fallbackVolumes
	if preferCurrent && len(preferredVolumes) > 0 {
		volumes = preferredVolumes
	}

	if len(volumes) == 0 {
		return 0, false
	}

	var total float32
	for _, volume := range volumes {
		total += volume
	}

	return total / float32(len(volumes)), true
}

// sliderMute reports the current mute state for all sessions mapped to a slider.
func (m *sessionMap) sliderMute(sliderIdx int) (bool, bool) {
	targets, ok := m.deej.config.SliderMapping.get(sliderIdx)
	if !ok || len(targets) == 0 {
		return false, false
	}

	preferredMutes := make([]bool, 0)
	fallbackMutes := make([]bool, 0)
	preferCurrent := false

	m.lock.RLock()
	defer m.lock.RUnlock()

	for _, target := range targets {
		trimmedTarget := strings.ToLower(strings.TrimSpace(target))
		resolvedTargets := m.resolveTarget(target)

		for _, resolvedTarget := range resolvedTargets {
			sessions, ok := m.m[resolvedTarget]
			if !ok || len(sessions) == 0 {
				continue
			}

			for _, session := range sessions {
				muted := session.GetMute()
				if trimmedTarget == specialTargetTransformPrefix+specialTargetCurrentWindow {
					preferCurrent = true
					preferredMutes = append(preferredMutes, muted)
				} else {
					fallbackMutes = append(fallbackMutes, muted)
				}
			}
		}
	}

	mutes := fallbackMutes
	if preferCurrent && len(preferredMutes) > 0 {
		mutes = preferredMutes
	}

	if len(mutes) == 0 {
		return false, false
	}

	for _, muted := range mutes {
		if muted {
			return true, true
		}
	}

	return false, true
}

// ToggleSliderMute toggles the mute state for all sessions mapped to a slider.
func (m *sessionMap) ToggleSliderMute(sliderIdx int) error {
	targets, ok := m.deej.config.SliderMapping.get(sliderIdx)
	if !ok || len(targets) == 0 {
		m.logger.Debugw("ToggleSliderMute: no targets mapped to slider", "slider", sliderIdx)
		return nil
	}

	currentMute, hasActive := m.sliderMute(sliderIdx)
	targetMute := true
	if hasActive && currentMute {
		targetMute = false
	}

	m.logger.Infow("Toggling slider mute", "slider", sliderIdx, "from", currentMute, "to", targetMute)

	adjustmentFailed := false
	targetFound := false

	m.lock.RLock()
	for _, target := range targets {
		resolvedTargets := m.resolveTarget(target)

		for _, resolvedTarget := range resolvedTargets {
			sessions, ok := m.m[resolvedTarget]
			if !ok || len(sessions) == 0 {
				continue
			}

			targetFound = true
			for _, session := range sessions {
				if err := session.SetMute(targetMute); err != nil {
					m.logger.Warnw("Failed to set target session mute", "session", session.Key(), "error", err)
					adjustmentFailed = true
				}
			}
		}
	}
	m.lock.RUnlock()

	if !targetFound || adjustmentFailed {
		m.refreshSessions(true)
	}

	if err := m.deej.serial.SendSliderMute(sliderIdx, targetMute); err != nil {
		m.logger.Warnw("Failed to send slider mute update", "slider", sliderIdx, "error", err)
	}

	return nil
}

func (m *sessionMap) setupSliderVolumeSync() {
	const syncInterval = 500 * time.Millisecond
	syncTicks := 0

	go func() {
		runtime.LockOSThread()
		_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if !m.deej.config.SyncVolumes {
					continue
				}
				syncTicks++
				// Periodically (every 30s = 60 ticks), check if any mapped targets were missing and discover new sessions
				if syncTicks%60 == 0 && m.hasMissingMappedTargets() {
					m.refreshSessions(false)
				}
				m.syncAllSliderVolumes()
			case <-m.sliderSyncStop:
				return
			}
		}
	}()
}

func (m *sessionMap) hasMissingMappedTargets() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	missing := false
	m.deej.config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		if missing {
			return
		}
		for _, target := range targets {
			if m.targetHasSpecialTransform(target) {
				continue
			}
			resolved := m.resolveTarget(target)
			for _, r := range resolved {
				if _, ok := m.m[r]; !ok {
					missing = true
					return
				}
			}
		}
	})
	return missing
}

func (m *sessionMap) syncAllSliderVolumes() {
	indices := []int{}

	m.deej.config.SliderMapping.iterate(func(sliderIdx int, _ []string) {
		indices = append(indices, sliderIdx)
	})

	if len(indices) == 0 {
		return
	}

	sort.Ints(indices)

	for _, idx := range indices {
		volume, ok := m.sliderVolume(idx)
		if ok {
			if err := m.deej.serial.SendSliderDisplayValue(idx, volume); err != nil {
				m.logger.Warnw("Failed to sync slider display", "slider", idx, "error", err)
			}
		}

		muted, ok := m.sliderMute(idx)
		if ok {
			if err := m.deej.serial.SendSliderMute(idx, muted); err != nil {
				m.logger.Warnw("Failed to sync slider mute", "slider", idx, "error", err)
			}
		}
	}
}

func (m *sessionMap) targetHasSpecialTransform(target string) bool {
	return strings.HasPrefix(target, specialTargetTransformPrefix)
}

func (m *sessionMap) resolveTarget(target string) []string {

	// start by ignoring the case
	target = strings.ToLower(target)

	// look for any special targets first, by examining the prefix
	if m.targetHasSpecialTransform(target) {
		return m.applyTargetTransform(strings.TrimPrefix(target, specialTargetTransformPrefix))
	}

	return []string{target}
}

func (m *sessionMap) applyTargetTransform(specialTargetName string) []string {

	// select the transformation based on its name
	switch specialTargetName {

	// get current active window
	case specialTargetCurrentWindow:
		currentWindowProcessNames, err := util.GetCurrentWindowProcessNames()

		// silently ignore errors here, as this is on deej's "hot path" (and it could just mean the user's running linux)
		if err != nil {
			return nil
		}

		// we could have gotten a non-lowercase names from that, so let's ensure we return ones that are lowercase
		for targetIdx, target := range currentWindowProcessNames {
			currentWindowProcessNames[targetIdx] = strings.ToLower(target)
		}

		// remove dupes
		return funk.UniqString(currentWindowProcessNames)

	// get currently unmapped sessions
	case specialTargetAllUnmapped:
		m.lock.RLock()
		targetKeys := make([]string, len(m.unmappedSessions))
		for sessionIdx, session := range m.unmappedSessions {
			targetKeys[sessionIdx] = session.Key()
		}
		m.lock.RUnlock()

		return targetKeys
	}

	return nil
}

func (m *sessionMap) add(value Session) {
	m.lock.Lock()
	defer m.lock.Unlock()

	key := value.Key()

	existing, ok := m.m[key]
	if !ok {
		m.m[key] = []Session{value}
	} else {
		m.m[key] = append(existing, value)
	}
}

func (m *sessionMap) get(key string) ([]Session, bool) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	value, ok := m.m[key]
	return value, ok
}

func (m *sessionMap) clear() {
	m.lock.Lock()
	var oldSessions []Session
	for _, sessions := range m.m {
		oldSessions = append(oldSessions, sessions...)
	}
	m.m = make(map[string][]Session)
	m.unmappedSessions = nil
	m.lock.Unlock()

	m.logger.Debug("Releasing and clearing all audio sessions")
	for _, session := range oldSessions {
		session.Release()
	}
	m.logger.Debug("Session map cleared")
}

func (m *sessionMap) String() string {
	m.lock.RLock()
	defer m.lock.RUnlock()

	sessionCount := 0

	for _, value := range m.m {
		sessionCount += len(value)
	}

	return fmt.Sprintf("<%d audio sessions>", sessionCount)
}

func (m *sessionMap) listSessionKeys() []string {
	m.lock.RLock()
	defer m.lock.RUnlock()

	keys := make([]string, 0, len(m.m))
	for key := range m.m {
		keys = append(keys, key)
	}

	sort.Strings(keys)
	return keys
}
