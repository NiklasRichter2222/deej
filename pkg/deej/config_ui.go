package deej

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v2"

	"github.com/omriharel/deej/pkg/deej/util"
)

const (
	configUIProfilesDir      = "profiles"
	configUIDefaultSliderCap = 32
)

var (
	profileNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	hexColorPattern    = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

type configUIService struct {
	deej   *Deej
	logger *zap.SugaredLogger

	mu      sync.Mutex
	started bool
	url     string

	listener net.Listener
	server   *http.Server
}

type configUIStateResponse struct {
	Config          configUIConfig          `json:"config"`
	Applications    []string                `json:"applications"`
	SerialPorts     []configUIPortOption    `json:"serialPorts"`
	Profiles        []string                `json:"profiles"`
	SpecialTargets  []string                `json:"specialTargets"`
	BaudRateOptions []int                   `json:"baudRateOptions"`
	ColorPresets    []configUIColorPreset   `json:"colorPresets"`
	BgPresets       []configUIBackgroundOpt `json:"bgPresets"`
}

type configUIConfig struct {
	SliderCount        int                               `json:"sliderCount"`
	SliderMapping      map[string][]string               `json:"sliderMapping"`
	COMPort            string                            `json:"comPort"`
	BaudRate           int                               `json:"baudRate"`
	InvertSliders      bool                              `json:"invertSliders"`
	NoiseReduction     string                            `json:"noiseReduction"`
	SendOnStartup      bool                              `json:"sendOnStartup"`
	SyncVolumes        bool                              `json:"syncVolumes"`
	BackgroundLighting string                            `json:"backgroundLighting"`
	ColorMapping       map[string]configUISliderColorMap `json:"colorMapping"`
	Commands           interface{}                       `json:"commands,omitempty"`
}

type configUISliderColorMap struct {
	Mode string `json:"mode"`
	Zero string `json:"zero"`
	Full string `json:"full"`
}

type configUIPortOption struct {
	Port        string `json:"port"`
	Description string `json:"description"`
}

type configUIColorPreset struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type configUIBackgroundOpt struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type configUISaveRequest struct {
	Config configUIConfig `json:"config"`
}

type configUIProfileRequest struct {
	Name string         `json:"name"`
	Save configUIConfig `json:"config"`
}

func newConfigUIService(d *Deej, logger *zap.SugaredLogger) *configUIService {
	return &configUIService{
		deej:   d,
		logger: logger.Named("config-ui"),
	}
}

func (s *configUIService) Open() error {
	s.mu.Lock()
	if !s.started {
		if err := s.startLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	url := s.url
	s.mu.Unlock()

	if err := util.OpenURL(s.logger, url); err != nil {
		return fmt.Errorf("open config ui in browser: %w", err)
	}

	return nil
}

func (s *configUIService) Stop() {
	s.mu.Lock()
	server := s.server
	s.server = nil
	s.listener = nil
	s.started = false
	s.url = ""
	s.mu.Unlock()

	if server == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		s.logger.Debugw("Failed to shutdown config UI server cleanly", "error", err)
	}
}

func (s *configUIService) startLocked() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/save", s.handleSave)
	mux.HandleFunc("/api/profiles/save", s.handleSaveProfile)
	mux.HandleFunc("/api/profiles/load", s.handleLoadProfile)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for config ui: %w", err)
	}

	s.listener = listener
	s.server = &http.Server{Handler: mux}
	s.url = fmt.Sprintf("http://%s", listener.Addr().String())
	s.started = true

	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Warnw("Config UI server stopped unexpectedly", "error", err)
		}
	}()

	s.logger.Infow("Started configuration UI server", "url", s.url)
	return nil
}

func (s *configUIService) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(configUIHTML))
}

func (s *configUIService) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := configUIStateResponse{
		Config:          s.currentConfig(),
		Applications:    s.applicationTargets(),
		SerialPorts:     listSerialPorts(),
		Profiles:        listProfiles(),
		SpecialTargets:  []string{"master", "mic", "system", "deej.current", "deej.unmapped"},
		BaudRateOptions: []int{9600, 19200, 38400, 57600, 115200, 230400},
		ColorPresets: []configUIColorPreset{
			{Name: "Red", Value: "#ff0000"},
			{Name: "Green", Value: "#00ff00"},
			{Name: "Blue", Value: "#0000ff"},
			{Name: "Yellow", Value: "#ffff00"},
			{Name: "Cyan", Value: "#00ffff"},
			{Name: "Magenta", Value: "#ff00ff"},
			{Name: "White", Value: "#ffffff"},
		},
		BgPresets: []configUIBackgroundOpt{
			{Name: "RGB rainbow", Value: "rgb"},
			{Name: "Off", Value: "off"},
			{Name: "Custom", Value: "custom"},
		},
	}

	writeJSON(w, http.StatusOK, state)
}

func (s *configUIService) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req configUISaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if err := saveConfigToPath(req.Config, userConfigFilepath); err != nil {
		s.logger.Warnw("Failed to save config from UI", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

func (s *configUIService) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req configUIProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	name, err := sanitizeProfileName(req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(configUIProfilesDir, os.ModePerm); err != nil {
		http.Error(w, "failed creating profiles directory", http.StatusInternalServerError)
		return
	}

	profilePath := filepath.Join(configUIProfilesDir, name+".yaml")
	if err := saveConfigToPath(req.Save, profilePath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

func (s *configUIService) handleLoadProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req configUIProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	name, err := sanitizeProfileName(req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	profilePath := filepath.Join(configUIProfilesDir, name+".yaml")
	content, err := ioutil.ReadFile(profilePath)
	if err != nil {
		http.Error(w, "failed reading selected profile", http.StatusBadRequest)
		return
	}

	if err := ioutil.WriteFile(userConfigFilepath, content, 0644); err != nil {
		http.Error(w, "failed writing config.yaml", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"loaded": true})
}

func (s *configUIService) currentConfig() configUIConfig {
	cfg := configUIConfig{
		SliderCount:        s.deej.config.SliderCount,
		SliderMapping:      map[string][]string{},
		COMPort:            s.deej.config.ConnectionInfo.COMPort,
		BaudRate:           s.deej.config.ConnectionInfo.BaudRate,
		InvertSliders:      s.deej.config.InvertSliders,
		NoiseReduction:     s.deej.config.NoiseReductionLevel,
		SendOnStartup:      s.deej.config.SendOnStartup,
		SyncVolumes:        s.deej.config.SyncVolumes,
		BackgroundLighting: s.deej.config.BackgroundLighting,
		ColorMapping:       map[string]configUISliderColorMap{},
		Commands:           s.deej.config.userConfig.Get(configKeyCommands),
	}

	maxIndex := -1
	s.deej.config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		cleanTargets := make([]string, len(targets))
		copy(cleanTargets, targets)
		cfg.SliderMapping[strconv.Itoa(sliderIdx)] = cleanTargets
		if sliderIdx > maxIndex {
			maxIndex = sliderIdx
		}
	})

	for idx, entry := range s.deej.config.ColorMapping {
		mode := "gradient"
		if strings.EqualFold(strings.TrimSpace(entry.Zero), strings.TrimSpace(entry.Full)) {
			mode = "single"
		}

		cfg.ColorMapping[strconv.Itoa(idx)] = configUISliderColorMap{
			Mode: mode,
			Zero: entry.Zero,
			Full: entry.Full,
		}
		if idx > maxIndex {
			maxIndex = idx
		}
	}

	if cfg.SliderCount <= 0 {
		cfg.SliderCount = maxIndex + 1
	}
	if cfg.SliderCount <= 0 {
		cfg.SliderCount = defaultSliders
	}
	if cfg.SliderCount > configUIDefaultSliderCap {
		cfg.SliderCount = configUIDefaultSliderCap
	}

	return cfg
}

func (s *configUIService) applicationTargets() []string {
	s.deej.sessions.refreshSessions(true)
	keys := s.deej.sessions.listSessionKeys()
	result := make([]string, 0, len(keys))

	for _, key := range keys {
		if key == masterSessionName || key == inputSessionName || key == systemSessionName {
			continue
		}
		result = append(result, key)
	}

	sort.Strings(result)
	return result
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func saveConfigToPath(config configUIConfig, targetPath string) error {
	sliderCount := config.SliderCount
	if sliderCount <= 0 {
		sliderCount = defaultSliders
	}
	if sliderCount > configUIDefaultSliderCap {
		sliderCount = configUIDefaultSliderCap
	}

	mapping := make(map[int][]string)
	for idx := 0; idx < sliderCount; idx++ {
		mapping[idx] = normalizeTargets(config.SliderMapping[strconv.Itoa(idx)])
	}

	colorMapping := make(map[int]SliderColorConfig)
	for idx := 0; idx < sliderCount; idx++ {
		entry, ok := config.ColorMapping[strconv.Itoa(idx)]
		if !ok {
			continue
		}

		zero := normalizeColor(entry.Zero)
		full := normalizeColor(entry.Full)
		if strings.EqualFold(entry.Mode, "single") {
			full = zero
		}

		if !isHexColor(zero) || !isHexColor(full) {
			continue
		}

		colorMapping[idx] = SliderColorConfig{Zero: zero, Full: full}
	}

	backgroundLighting := strings.TrimSpace(config.BackgroundLighting)
	if strings.EqualFold(backgroundLighting, "custom") {
		backgroundLighting = ""
	}
	if backgroundLighting != "" && !strings.EqualFold(backgroundLighting, "rgb") && !strings.EqualFold(backgroundLighting, "off") && !isHexColor(backgroundLighting) {
		backgroundLighting = ""
	}

	noiseReduction := strings.TrimSpace(strings.ToLower(config.NoiseReduction))
	if noiseReduction == "" {
		noiseReduction = "default"
	}
	if noiseReduction != "low" && noiseReduction != "default" && noiseReduction != "high" {
		noiseReduction = "default"
	}

	buf := &bytes.Buffer{}
	buf.WriteString("# --- Slider Mapping ---\n")
	buf.WriteString("# Process names are case-insensitive.\n")
	buf.WriteString("# Supported special targets: master, mic, system, deej.current, deej.unmapped.\n")
	fmt.Fprintf(buf, "slider_count: %d\n", sliderCount)
	buf.WriteString("slider_mapping:\n")

	for idx := 0; idx < sliderCount; idx++ {
		targets := mapping[idx]
		switch len(targets) {
		case 0:
			fmt.Fprintf(buf, "  %d: []\n", idx)
		case 1:
			fmt.Fprintf(buf, "  %d: %s\n", idx, yamlString(targets[0]))
		default:
			fmt.Fprintf(buf, "  %d:\n", idx)
			for _, target := range targets {
				fmt.Fprintf(buf, "    - %s\n", yamlString(target))
			}
		}
	}

	buf.WriteString("\n# --- General Options ---\n")
	fmt.Fprintf(buf, "invert_sliders: %t\n", config.InvertSliders)
	fmt.Fprintf(buf, "noise_reduction: %s\n", yamlString(noiseReduction))

	buf.WriteString("\n# --- Connection Settings ---\n")
	comPort := strings.TrimSpace(config.COMPort)
	if comPort == "" {
		comPort = defaultCOMPort
	}
	fmt.Fprintf(buf, "com_port: %s\n", yamlString(comPort))
	fmt.Fprintf(buf, "baud_rate: %d\n", normalizeBaudRate(config.BaudRate))

	buf.WriteString("\n# --- Bidirectional Sync Settings ---\n")
	fmt.Fprintf(buf, "send_on_startup: %t\n", config.SendOnStartup)
	fmt.Fprintf(buf, "sync_volumes: %t\n", config.SyncVolumes)

	buf.WriteString("\n# --- Controller Lighting ---\n")
	fmt.Fprintf(buf, "background_lighting: %s\n", yamlString(backgroundLighting))
	buf.WriteString("color_mapping:\n")
	if len(colorMapping) == 0 {
		buf.WriteString("  {}\n")
	} else {
		indices := make([]int, 0, len(colorMapping))
		for idx := range colorMapping {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			entry := colorMapping[idx]
			fmt.Fprintf(buf, "  %d:\n", idx)
			fmt.Fprintf(buf, "    zero: %s\n", yamlString(entry.Zero))
			fmt.Fprintf(buf, "    full: %s\n", yamlString(entry.Full))
		}
	}

	if config.Commands != nil {
		buf.WriteString("\n# --- Commands (not edited in UI) ---\n")
		commandsDoc, err := yaml.Marshal(map[string]interface{}{configKeyCommands: config.Commands})
		if err == nil {
			buf.Write(commandsDoc)
		}
	}

	return ioutil.WriteFile(targetPath, buf.Bytes(), 0644)
}

func normalizeTargets(raw []string) []string {
	cleaned := []string{}
	seen := make(map[string]bool)

	for _, target := range raw {
		trimmed := strings.TrimSpace(target)
		if trimmed == "" || strings.ContainsAny(trimmed, "\r\n") {
			continue
		}

		key := strings.ToLower(trimmed)
		if seen[key] {
			continue
		}

		seen[key] = true
		cleaned = append(cleaned, trimmed)
	}

	return cleaned
}

func normalizeColor(value string) string {
	v := strings.TrimSpace(value)
	if strings.HasPrefix(v, "#") {
		return strings.ToLower(v)
	}
	return v
}

func isHexColor(value string) bool {
	return hexColorPattern.MatchString(strings.TrimSpace(value))
}

func normalizeBaudRate(v int) int {
	if v <= 0 {
		return defaultBaudRate
	}
	return v
}

func yamlString(value string) string {
	bytesValue, err := yaml.Marshal(value)
	if err != nil {
		return "\"\""
	}

	return strings.TrimSpace(string(bytesValue))
}

func sanitizeProfileName(name string) (string, error) {
	clean := strings.TrimSpace(name)
	clean = strings.TrimSuffix(clean, ".yaml")
	if clean == "" {
		return "", fmt.Errorf("profile name is required")
	}
	if !profileNamePattern.MatchString(clean) {
		return "", fmt.Errorf("profile name can only contain letters, numbers, dot, dash and underscore")
	}
	return clean, nil
}

func listProfiles() []string {
	paths, err := filepath.Glob(filepath.Join(configUIProfilesDir, "*.yaml"))
	if err != nil {
		return []string{}
	}

	profiles := make([]string, 0, len(paths))
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		if name != "" {
			profiles = append(profiles, name)
		}
	}

	sort.Strings(profiles)
	return profiles
}

func listSerialPorts() []configUIPortOption {
	if util.Windows() {
		return listSerialPortsWindows()
	}

	if util.Linux() {
		return listSerialPortsLinux()
	}

	return []configUIPortOption{}
}

func listSerialPortsWindows() []configUIPortOption {
	command := exec.Command("powershell", "-NoProfile", "-Command", "Get-CimInstance Win32_SerialPort | Select-Object DeviceID,Name | ConvertTo-Json -Compress")
	output, err := command.Output()
	if err == nil {
		type winSerialPort struct {
			DeviceID string `json:"DeviceID"`
			Name     string `json:"Name"`
		}

		ports := []configUIPortOption{}

		if len(output) > 0 {
			var many []winSerialPort
			if err := json.Unmarshal(output, &many); err == nil {
				for _, port := range many {
					ports = append(ports, configUIPortOption{Port: port.DeviceID, Description: port.Name})
				}
			} else {
				var single winSerialPort
				if err := json.Unmarshal(output, &single); err == nil && single.DeviceID != "" {
					ports = append(ports, configUIPortOption{Port: single.DeviceID, Description: single.Name})
				}
			}
		}

		if len(ports) > 0 {
			sort.Slice(ports, func(i int, j int) bool {
				return ports[i].Port < ports[j].Port
			})
			return ports
		}
	}

	fallback := exec.Command("powershell", "-NoProfile", "-Command", "[System.IO.Ports.SerialPort]::GetPortNames() | Sort-Object")
	fallbackOutput, err := fallback.Output()
	if err != nil {
		return []configUIPortOption{}
	}

	lines := strings.Split(strings.TrimSpace(string(fallbackOutput)), "\n")
	ports := make([]configUIPortOption, 0, len(lines))
	for _, line := range lines {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		ports = append(ports, configUIPortOption{Port: name, Description: name})
	}

	return ports
}

func listSerialPortsLinux() []configUIPortOption {
	patterns := []string{"/dev/ttyACM*", "/dev/ttyUSB*"}
	paths := []string{}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		paths = append(paths, matches...)
	}

	sort.Strings(paths)

	ports := make([]configUIPortOption, 0, len(paths))
	for _, path := range paths {
		base := filepath.Base(path)
		description := readFirstExisting(
			filepath.Join("/sys/class/tty", base, "device/interface"),
			filepath.Join("/sys/class/tty", base, "device/product"),
		)
		if description == "" {
			description = base
		}

		ports = append(ports, configUIPortOption{
			Port:        path,
			Description: description,
		})
	}

	return ports
}

func readFirstExisting(paths ...string) string {
	for _, path := range paths {
		content, err := ioutil.ReadFile(path)
		if err != nil {
			continue
		}

		trimmed := strings.TrimSpace(string(content))
		if trimmed != "" {
			return trimmed
		}
	}

	return ""
}
