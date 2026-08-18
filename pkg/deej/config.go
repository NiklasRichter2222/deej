package deej

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// CanonicalConfig provides application-wide access to configuration fields,
// as well as loading/file watching logic for deej's configuration file
type SliderColorConfig struct {
	Zero string `mapstructure:"zero"`
	Full string `mapstructure:"full"`
}

type CanonicalConfig struct {
	SliderMappingLeft  *sliderMap
	SliderMappingRight *sliderMap
	SliderMapping      *sliderMap // points to current active page slider map
	SliderCount        int

	ActivePage string // "left" or "right"

	ConnectionInfo struct {
		COMPort  string
		BaudRate int
	}

	InvertSliders bool

	NoiseReductionLevel string

	DefaultBrightness  float64
	SendOnStartup      bool
	SyncVolumes        bool
	ColorMappingLeft   map[int]SliderColorConfig
	ColorMappingRight  map[int]SliderColorConfig
	ColorMapping       map[int]SliderColorConfig // points to current active page color map
	BackgroundLighting string

	PageButtonLeftColor     string
	PageButtonLeftOffColor  string
	PageButtonRightColor    string
	PageButtonRightOffColor string

	logger             *zap.SugaredLogger
	notifier           Notifier
	stopWatcherChannel chan bool

	reloadConsumers []chan bool

	userConfig     *viper.Viper
	internalConfig *viper.Viper

	configFingerprintMu sync.Mutex
	configFingerprint   string
}

const (
	userConfigFilepath     = "config.yaml"
	internalConfigFilepath = "preferences.yaml"

	userConfigName     = "config"
	internalConfigName = "preferences"

	userConfigPath = "."

	configType = "yaml"

	configKeySliderMapping       = "slider_mapping"
	configKeySliderCount         = "slider_count"
	configKeyInvertSliders       = "invert_sliders"
	configKeyCOMPort             = "com_port"
	configKeyBaudRate            = "baud_rate"
	configKeyNoiseReductionLevel = "noise_reduction"
	configKeyDefaultBrightness   = "default_brightness"
	configKeySendOnStartup       = "send_on_startup"
	configKeySyncVolumes         = "sync_volumes"
	configKeyColorMapping        = "color_mapping"
	configKeyBackgroundLighting  = "background_lighting"

	defaultCOMPort  = "COM4"
	defaultBaudRate = 9600
	defaultSliders  = 6
)

var internalConfigPath = path.Join(".", logDirectory)

var defaultSliderMapping = func() *sliderMap {
	emptyMap := newSliderMap()
	emptyMap.set(0, []string{masterSessionName})

	return emptyMap
}()

// NewConfig creates a config instance for the deej object and sets up viper instances for deej's config files
func NewConfig(logger *zap.SugaredLogger, notifier Notifier) (*CanonicalConfig, error) {
	logger = logger.Named("config")

	cc := &CanonicalConfig{
		logger:                  logger,
		notifier:                notifier,
		ActivePage:              "left",
		DefaultBrightness:       0.15,
		PageButtonLeftColor:     "#ffffff",
		PageButtonLeftOffColor:  "#000000",
		PageButtonRightColor:    "#ffffff",
		PageButtonRightOffColor: "#000000",
		reloadConsumers:         []chan bool{},
		stopWatcherChannel:      make(chan bool),
	}

	userConfig := viper.New()
	userConfig.SetConfigName(userConfigName)
	userConfig.SetConfigType(configType)
	userConfig.AddConfigPath(userConfigPath)

	userConfig.SetDefault(configKeySliderMapping, map[string]interface{}{})
	userConfig.SetDefault(configKeySliderCount, 0)
	userConfig.SetDefault(configKeyInvertSliders, false)
	userConfig.SetDefault(configKeyCOMPort, defaultCOMPort)
	userConfig.SetDefault(configKeyBaudRate, defaultBaudRate)
	userConfig.SetDefault(configKeyDefaultBrightness, 0.15)
	userConfig.SetDefault(configKeySendOnStartup, false)
	userConfig.SetDefault(configKeySyncVolumes, false)
	userConfig.SetDefault(configKeyColorMapping, map[string]interface{}{})
	userConfig.SetDefault(configKeyBackgroundLighting, "")

	internalConfig := viper.New()
	internalConfig.SetConfigName(internalConfigName)
	internalConfig.SetConfigType(configType)
	internalConfig.AddConfigPath(internalConfigPath)

	cc.userConfig = userConfig
	cc.internalConfig = internalConfig

	logger.Debug("Created config instance")

	return cc, nil
}

// Load reads deej's config files from disk and tries to parse them
func (cc *CanonicalConfig) Load() error {
	cc.logger.Debugw("Loading config", "path", userConfigFilepath)

	if !util.FileExists(userConfigFilepath) {
		cc.logger.Warnw("Config file not found", "path", userConfigFilepath)
		return fmt.Errorf("config file not found: %s", userConfigFilepath)
	}

	if err := cc.userConfig.ReadInConfig(); err != nil {
		cc.logger.Errorw("Failed to read user config file", "error", err)
		return fmt.Errorf("read user config file: %w", err)
	}

	if util.FileExists(path.Join(internalConfigPath, internalConfigFilepath)) {
		if err := cc.internalConfig.ReadInConfig(); err != nil {
			cc.logger.Errorw("Failed to read internal config file", "error", err)
			return fmt.Errorf("read internal config file: %w", err)
		}
	}

	if err := cc.populateFromVipers(); err != nil {
		cc.logger.Errorw("Failed to populate config fields", "error", err)
		return fmt.Errorf("populate config fields: %w", err)
	}

	cc.captureConfigFingerprint()
	cc.logger.Info("Loaded config successfully")

	return nil
}

func (cc *CanonicalConfig) SubscribeToChanges() chan bool {
	c := make(chan bool)
	cc.reloadConsumers = append(cc.reloadConsumers, c)

	return c
}

func (cc *CanonicalConfig) WatchConfigFileChanges() {
	cc.logger.Debugw("Starting to watch user config file for changes", "path", userConfigFilepath)
	cc.userConfig.WatchConfig()

	const debounceDuration = 50 * time.Millisecond
	var debounceTimer *time.Timer

	cc.userConfig.OnConfigChange(func(event fsnotify.Event) {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}

		debounceTimer = time.AfterFunc(debounceDuration, func() {
			cc.logger.Debugw("Config file change detected", "event", event.Op.String())

			if !cc.configChangedSince(cc.currentConfigFingerprint()) {
				cc.logger.Debug("Config file change event was a false alarm, ignoring")
				return
			}

			cc.logger.Info("Config file changed, reloading")

			if err := cc.Load(); err != nil {
				cc.logger.Warnw("Failed to reload config file", "error", err)
				cc.notifier.Notify("Error reloading config!", "Please check deej.log for details.")
				return
			}

			cc.onConfigReloaded()
		})
	})

	<-cc.stopWatcherChannel
	cc.logger.Debug("Stopping user config file watcher")
	cc.userConfig.OnConfigChange(nil)
}

func (cc *CanonicalConfig) StopWatchingConfigFile() {
	cc.stopWatcherChannel <- true
}

func (cc *CanonicalConfig) SetActivePage(page string) {
	page = strings.ToLower(strings.TrimSpace(page))
	if page != "right" {
		page = "left"
	}
	cc.ActivePage = page
	if page == "right" {
		cc.SliderMapping = cc.SliderMappingRight
		cc.ColorMapping = cc.ColorMappingRight
	} else {
		cc.SliderMapping = cc.SliderMappingLeft
		cc.ColorMapping = cc.ColorMappingLeft
	}
}

func (cc *CanonicalConfig) populateFromVipers() error {
	leftSliderMap, rightSliderMap := cc.parseSliderMappings()
	cc.SliderMappingLeft = leftSliderMap
	cc.SliderMappingRight = rightSliderMap

	cc.ConnectionInfo.COMPort = cc.userConfig.GetString(configKeyCOMPort)
	cc.ConnectionInfo.BaudRate = cc.userConfig.GetInt(configKeyBaudRate)
	if cc.ConnectionInfo.BaudRate <= 0 {
		cc.logger.Warnw("Invalid baud rate specified, using default value",
			"key", configKeyBaudRate,
			"invalidValue", cc.ConnectionInfo.BaudRate,
			"defaultValue", defaultBaudRate)

		cc.ConnectionInfo.BaudRate = defaultBaudRate
	}

	cc.InvertSliders = cc.userConfig.GetBool(configKeyInvertSliders)
	cc.NoiseReductionLevel = cc.userConfig.GetString(configKeyNoiseReductionLevel)
	cc.SendOnStartup = cc.userConfig.GetBool(configKeySendOnStartup)
	cc.SyncVolumes = cc.userConfig.GetBool(configKeySyncVolumes)

	rawBrightness := cc.userConfig.GetFloat64(configKeyDefaultBrightness)
	if rawBrightness > 1.0 {
		rawBrightness = rawBrightness / 100.0
	}
	if rawBrightness < 0.0 {
		rawBrightness = 0.0
	} else if rawBrightness > 1.0 {
		rawBrightness = 1.0
	}
	if !cc.userConfig.IsSet(configKeyDefaultBrightness) {
		rawBrightness = 0.15
	}
	cc.DefaultBrightness = rawBrightness

	cLeft, cRight, btnLeft, btnLeftOff, btnRight, btnRightOff := cc.parseColorMappings()
	cc.ColorMappingLeft = cLeft
	cc.ColorMappingRight = cRight
	cc.PageButtonLeftColor = btnLeft
	cc.PageButtonLeftOffColor = btnLeftOff
	cc.PageButtonRightColor = btnRight
	cc.PageButtonRightOffColor = btnRightOff

	cc.SliderCount = cc.userConfig.GetInt(configKeySliderCount)
	if cc.SliderCount <= 0 {
		cc.SliderCount = cc.inferSliderCount()
	}
	cc.BackgroundLighting = strings.TrimSpace(cc.userConfig.GetString(configKeyBackgroundLighting))

	cc.SetActivePage(cc.ActivePage)

	cc.logger.Debug("Populated config fields from vipers")
	return nil
}

func (cc *CanonicalConfig) parseSliderMappings() (*sliderMap, *sliderMap) {
	userRaw := cc.userConfig.GetStringMap(configKeySliderMapping)
	_, hasLeft := userRaw["left"]
	_, hasRight := userRaw["right"]

	internalRaw := cc.internalConfig.GetStringMap(configKeySliderMapping)
	_, intHasLeft := internalRaw["left"]
	_, intHasRight := internalRaw["right"]

	var userLeft, userRight map[string][]string
	var intLeft, intRight map[string][]string

	if hasLeft || hasRight {
		userLeft = cc.userConfig.GetStringMapStringSlice(configKeySliderMapping + ".left")
		userRight = cc.userConfig.GetStringMapStringSlice(configKeySliderMapping + ".right")
	} else {
		flat := cc.userConfig.GetStringMapStringSlice(configKeySliderMapping)
		userLeft = flat
		userRight = flat
	}

	if intHasLeft || intHasRight {
		intLeft = cc.internalConfig.GetStringMapStringSlice(configKeySliderMapping + ".left")
		intRight = cc.internalConfig.GetStringMapStringSlice(configKeySliderMapping + ".right")
	} else {
		flat := cc.internalConfig.GetStringMapStringSlice(configKeySliderMapping)
		intLeft = flat
		intRight = flat
	}

	leftMap := sliderMapFromConfigs(userLeft, intLeft)
	rightMap := sliderMapFromConfigs(userRight, intRight)

	return leftMap, rightMap
}

func (cc *CanonicalConfig) parseColorMappings() (map[int]SliderColorConfig, map[int]SliderColorConfig, string, string, string, string) {
	leftResult := make(map[int]SliderColorConfig)
	rightResult := make(map[int]SliderColorConfig)

	leftColor := "#ffffff"
	leftOffColor := "#000000"
	rightColor := "#ffffff"
	rightOffColor := "#000000"

	raw := cc.userConfig.GetStringMap(configKeyColorMapping)
	_, hasLeft := raw["left"]
	_, hasRight := raw["right"]

	if hasLeft || hasRight {
		if hasLeft {
			if lColor := cc.userConfig.GetString(configKeyColorMapping + ".left.color"); lColor != "" {
				leftColor = lColor
			}
			if lOff := cc.userConfig.GetString(configKeyColorMapping + ".left.offcolor"); lOff != "" {
				leftOffColor = lOff
			}
			leftRaw := make(map[string]SliderColorConfig)
			_ = cc.userConfig.UnmarshalKey(configKeyColorMapping+".left", &leftRaw)
			for key, entry := range leftRaw {
				if key == "color" || key == "offcolor" {
					continue
				}
				if entry.Zero == "" && entry.Full == "" {
					continue
				}
				idx, err := strconv.Atoi(strings.TrimSpace(key))
				if err != nil {
					continue
				}
				leftResult[idx] = SliderColorConfig{Zero: strings.TrimSpace(entry.Zero), Full: strings.TrimSpace(entry.Full)}
			}
		}
		if hasRight {
			if rColor := cc.userConfig.GetString(configKeyColorMapping + ".right.color"); rColor != "" {
				rightColor = rColor
			}
			if rOff := cc.userConfig.GetString(configKeyColorMapping + ".right.offcolor"); rOff != "" {
				rightOffColor = rOff
			}
			rightRaw := make(map[string]SliderColorConfig)
			_ = cc.userConfig.UnmarshalKey(configKeyColorMapping+".right", &rightRaw)
			for key, entry := range rightRaw {
				if key == "color" || key == "offcolor" {
					continue
				}
				if entry.Zero == "" && entry.Full == "" {
					continue
				}
				idx, err := strconv.Atoi(strings.TrimSpace(key))
				if err != nil {
					continue
				}
				rightResult[idx] = SliderColorConfig{Zero: strings.TrimSpace(entry.Zero), Full: strings.TrimSpace(entry.Full)}
			}
		}
	} else {
		flatRaw := make(map[string]SliderColorConfig)
		_ = cc.userConfig.UnmarshalKey(configKeyColorMapping, &flatRaw)
		for key, entry := range flatRaw {
			if entry.Zero == "" && entry.Full == "" {
				continue
			}
			idx, err := strconv.Atoi(strings.TrimSpace(key))
			if err != nil {
				continue
			}
			leftResult[idx] = SliderColorConfig{Zero: strings.TrimSpace(entry.Zero), Full: strings.TrimSpace(entry.Full)}
			rightResult[idx] = SliderColorConfig{Zero: strings.TrimSpace(entry.Zero), Full: strings.TrimSpace(entry.Full)}
		}
	}

	return leftResult, rightResult, leftColor, leftOffColor, rightColor, rightOffColor
}

func (cc *CanonicalConfig) inferSliderCount() int {
	maxSliderIdx := -1
	if cc.SliderMappingLeft != nil {
		cc.SliderMappingLeft.iterate(func(sliderIdx int, _ []string) {
			if sliderIdx > maxSliderIdx {
				maxSliderIdx = sliderIdx
			}
		})
	}
	if cc.SliderMappingRight != nil {
		cc.SliderMappingRight.iterate(func(sliderIdx int, _ []string) {
			if sliderIdx > maxSliderIdx {
				maxSliderIdx = sliderIdx
			}
		})
	}

	for sliderIdx := range cc.ColorMappingLeft {
		if sliderIdx > maxSliderIdx {
			maxSliderIdx = sliderIdx
		}
	}
	for sliderIdx := range cc.ColorMappingRight {
		if sliderIdx > maxSliderIdx {
			maxSliderIdx = sliderIdx
		}
	}

	if maxSliderIdx >= 0 {
		return maxSliderIdx + 1
	}

	return defaultSliders
}

func (cc *CanonicalConfig) onConfigReloaded() {
	cc.logger.Debug("Notifying consumers about configuration reload")

	for _, consumer := range cc.reloadConsumers {
		consumer <- true
	}
}

func (cc *CanonicalConfig) captureConfigFingerprint() {
	content, err := ioutil.ReadFile(userConfigFilepath)
	if err != nil {
		cc.logger.Debugw("Failed to capture config fingerprint", "error", err)
		return
	}

	sum := sha1.Sum(content)
	fingerprint := hex.EncodeToString(sum[:])

	cc.configFingerprintMu.Lock()
	cc.configFingerprint = fingerprint
	cc.configFingerprintMu.Unlock()
}

func (cc *CanonicalConfig) configChangedSince(previous string) bool {
	current := cc.currentConfigFingerprint()

	if previous == "" || current == "" {
		return true
	}

	return previous != current
}

func (cc *CanonicalConfig) currentConfigFingerprint() string {
	cc.configFingerprintMu.Lock()
	defer cc.configFingerprintMu.Unlock()

	return cc.configFingerprint
}
