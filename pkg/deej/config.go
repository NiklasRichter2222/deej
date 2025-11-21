package deej

import (
	"fmt"
	"path"
	"strconv"
	"strings"
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

type CommandSpec struct {
	Args  []string
	Shell bool
}

type CanonicalConfig struct {
	SliderMapping *sliderMap

	ConnectionInfo struct {
		COMPort  string
		BaudRate int
	}

	InvertSliders bool

	NoiseReductionLevel string

	SendOnStartup      bool
	SyncVolumes        bool
	ColorMapping       map[int]SliderColorConfig
	BackgroundLighting string
	Commands           map[int]CommandSpec

	logger             *zap.SugaredLogger
	notifier           Notifier
	stopWatcherChannel chan bool

	reloadConsumers []chan bool

	userConfig     *viper.Viper
	internalConfig *viper.Viper
}

const (
	userConfigFilepath     = "config.yaml"
	internalConfigFilepath = "preferences.yaml"

	userConfigName     = "config"
	internalConfigName = "preferences"

	userConfigPath = "."

	configType = "yaml"

	configKeySliderMapping       = "slider_mapping"
	configKeyInvertSliders       = "invert_sliders"
	configKeyCOMPort             = "com_port"
	configKeyBaudRate            = "baud_rate"
	configKeyNoiseReductionLevel = "noise_reduction"
	configKeySendOnStartup       = "send_on_startup"
	configKeySyncVolumes         = "sync_volumes"
	configKeyColorMapping        = "color_mapping"
	configKeyBackgroundLighting  = "background_lighting"
	configKeyCommands            = "commands"

	defaultCOMPort  = "COM4"
	defaultBaudRate = 9600
)

// has to be defined as a non-constant because we're using path.Join
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
		logger:             logger,
		notifier:           notifier,
		reloadConsumers:    []chan bool{},
		stopWatcherChannel: make(chan bool),
	}

	// distinguish between the user-provided config (config.yaml) and the internal config (logs/preferences.yaml)
	userConfig := viper.New()
	userConfig.SetConfigName(userConfigName)
	userConfig.SetConfigType(configType)
	userConfig.AddConfigPath(userConfigPath)

	userConfig.SetDefault(configKeySliderMapping, map[string][]string{})
	userConfig.SetDefault(configKeyInvertSliders, false)
	userConfig.SetDefault(configKeyCOMPort, defaultCOMPort)
	userConfig.SetDefault(configKeyBaudRate, defaultBaudRate)
	userConfig.SetDefault(configKeySendOnStartup, false)
	userConfig.SetDefault(configKeySyncVolumes, false)
	userConfig.SetDefault(configKeyColorMapping, map[string]map[string]string{})
	userConfig.SetDefault(configKeyBackgroundLighting, "")
	userConfig.SetDefault(configKeyCommands, map[string]interface{}{})

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

	// make sure it exists
	if !util.FileExists(userConfigFilepath) {
		cc.logger.Warnw("Config file not found", "path", userConfigFilepath)
		cc.notifier.Notify("Can't find configuration!",
			fmt.Sprintf("%s must be in the same directory as deej. Please re-launch", userConfigFilepath))

		return fmt.Errorf("config file doesn't exist: %s", userConfigFilepath)
	}

	// load the user config
	if err := cc.userConfig.ReadInConfig(); err != nil {
		cc.logger.Warnw("Viper failed to read user config", "error", err)

		// if the error is yaml-format-related, show a sensible error. otherwise, show 'em to the logs
		if strings.Contains(err.Error(), "yaml:") {
			cc.notifier.Notify("Invalid configuration!",
				fmt.Sprintf("Please make sure %s is in a valid YAML format.", userConfigFilepath))
		} else {
			cc.notifier.Notify("Error loading configuration!", "Please check deej's logs for more details.")
		}

		return fmt.Errorf("read user config: %w", err)
	}

	// load the internal config - this doesn't have to exist, so it can error
	if err := cc.internalConfig.ReadInConfig(); err != nil {
		cc.logger.Debugw("Viper failed to read internal config", "error", err, "reminder", "this is fine")
	}

	// canonize the configuration with viper's helpers
	if err := cc.populateFromVipers(); err != nil {
		cc.logger.Warnw("Failed to populate config fields", "error", err)
		return fmt.Errorf("populate config fields: %w", err)
	}

	cc.logger.Info("Loaded config successfully")
	cc.logger.Infow("Config values",
		"sliderMapping", cc.SliderMapping,
		"connectionInfo", cc.ConnectionInfo,
		"invertSliders", cc.InvertSliders)

	return nil
}

// SubscribeToChanges allows external components to receive updates when the config is reloaded
func (cc *CanonicalConfig) SubscribeToChanges() chan bool {
	c := make(chan bool)
	cc.reloadConsumers = append(cc.reloadConsumers, c)

	return c
}

// WatchConfigFileChanges starts watching for configuration file changes
// and attempts reloading the config when they happen
func (cc *CanonicalConfig) WatchConfigFileChanges() {
	cc.logger.Debugw("Starting to watch user config file for changes", "path", userConfigFilepath)

	const (
		minTimeBetweenReloadAttempts = time.Millisecond * 500
		delayBetweenEventAndReload   = time.Millisecond * 50
	)

	lastAttemptedReload := time.Now()

	// establish watch using viper as opposed to doing it ourselves, though our internal cooldown is still required
	cc.userConfig.WatchConfig()
	cc.userConfig.OnConfigChange(func(event fsnotify.Event) {

		// when we get a write event...
		if event.Op&fsnotify.Write == fsnotify.Write {

			now := time.Now()

			// ... check if it's not a duplicate (many editors will write to a file twice)
			if lastAttemptedReload.Add(minTimeBetweenReloadAttempts).Before(now) {

				// and attempt reload if appropriate
				cc.logger.Debugw("Config file modified, attempting reload", "event", event)

				// wait a bit to let the editor actually flush the new file contents to disk
				<-time.After(delayBetweenEventAndReload)

				if err := cc.Load(); err != nil {
					cc.logger.Warnw("Failed to reload config file", "error", err)
				} else {
					cc.logger.Info("Reloaded config successfully")
					cc.notifier.Notify("Configuration reloaded!", "Your changes have been applied.")

					cc.onConfigReloaded()
				}

				// don't forget to update the time
				lastAttemptedReload = now
			}
		}
	})

	// wait till they stop us
	<-cc.stopWatcherChannel
	cc.logger.Debug("Stopping user config file watcher")
	cc.userConfig.OnConfigChange(nil)
}

// StopWatchingConfigFile signals our filesystem watcher to stop
func (cc *CanonicalConfig) StopWatchingConfigFile() {
	cc.stopWatcherChannel <- true
}

func (cc *CanonicalConfig) populateFromVipers() error {

	// merge the slider mappings from the user and internal configs
	cc.SliderMapping = sliderMapFromConfigs(
		cc.userConfig.GetStringMapStringSlice(configKeySliderMapping),
		cc.internalConfig.GetStringMapStringSlice(configKeySliderMapping),
	)

	// get the rest of the config fields - viper saves us a lot of effort here
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
	cc.ColorMapping = cc.parseColorMapping()
	cc.BackgroundLighting = strings.TrimSpace(cc.userConfig.GetString(configKeyBackgroundLighting))
	cc.Commands = cc.parseCommands()

	cc.logger.Debug("Populated config fields from vipers")

	return nil
}

func (cc *CanonicalConfig) onConfigReloaded() {
	cc.logger.Debug("Notifying consumers about configuration reload")

	for _, consumer := range cc.reloadConsumers {
		consumer <- true
	}
}

func (cc *CanonicalConfig) parseColorMapping() map[int]SliderColorConfig {
	result := make(map[int]SliderColorConfig)

	raw := make(map[string]SliderColorConfig)
	if err := cc.userConfig.UnmarshalKey(configKeyColorMapping, &raw); err != nil {
		cc.logger.Warnw("Failed to parse color mapping from config", "error", err)
		return result
	}

	for key, entry := range raw {
		if entry.Zero == "" && entry.Full == "" {
			continue
		}

		sliderIdx, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil {
			cc.logger.Warnw("Ignoring color mapping entry with non-numeric key", "key", key)
			continue
		}

		zero := strings.TrimSpace(entry.Zero)
		full := strings.TrimSpace(entry.Full)
		if zero == "" || full == "" {
			cc.logger.Warnw("Ignoring color mapping entry with missing colors", "key", key)
			continue
		}

		result[sliderIdx] = SliderColorConfig{Zero: zero, Full: full}
	}

	return result
}

func (cc *CanonicalConfig) parseCommands() map[int]CommandSpec {
	result := make(map[int]CommandSpec)

	raw := cc.userConfig.GetStringMap(configKeyCommands)
	for key, value := range raw {
		sliderIdx, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil {
			cc.logger.Warnw("Ignoring command entry with non-numeric key", "key", key)
			continue
		}

		spec, ok := cc.parseCommandValue(value)
		if !ok {
			continue
		}

		result[sliderIdx] = spec
	}

	return result
}

func (cc *CanonicalConfig) parseCommandValue(value interface{}) (CommandSpec, bool) {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return CommandSpec{}, false
		}

		return CommandSpec{
			Args:  []string{trimmed},
			Shell: true,
		}, true
	case []interface{}:
		args := []string{}
		for _, rawArg := range typed {
			strArg, ok := rawArg.(string)
			if !ok {
				cc.logger.Warnw("Ignoring command entry with non-string argument", "value", rawArg)
				return CommandSpec{}, false
			}

			trimmed := strings.TrimSpace(strArg)
			if trimmed == "" {
				continue
			}
			args = append(args, trimmed)
		}

		if len(args) == 0 {
			return CommandSpec{}, false
		}

		return CommandSpec{
			Args: args,
		}, true
	case []string:
		args := []string{}
		for _, rawArg := range typed {
			trimmed := strings.TrimSpace(rawArg)
			if trimmed == "" {
				continue
			}
			args = append(args, trimmed)
		}

		if len(args) == 0 {
			return CommandSpec{}, false
		}

		return CommandSpec{
			Args: args,
		}, true
	case map[string]interface{}:
		// allow optional explicit spec: { shell: true, args: [...] }
		return cc.parseCommandMap(typed)
	default:
		cc.logger.Warnw("Ignoring command entry with unsupported type", "value", value)
		return CommandSpec{}, false
	}
}

func (cc *CanonicalConfig) parseCommandMap(value map[string]interface{}) (CommandSpec, bool) {
	spec := CommandSpec{}

	if shellValue, ok := value["shell"]; ok {
		if shellBool, ok := shellValue.(bool); ok {
			spec.Shell = shellBool
		} else {
			cc.logger.Warnw("Ignoring command entry with non-bool shell flag", "value", shellValue)
			return CommandSpec{}, false
		}
	}

	if argsValue, ok := value["args"]; ok {
		switch typedArgs := argsValue.(type) {
		case []interface{}:
			for _, rawArg := range typedArgs {
				strArg, ok := rawArg.(string)
				if !ok {
					cc.logger.Warnw("Ignoring command entry with non-string argument", "value", rawArg)
					return CommandSpec{}, false
				}

				trimmed := strings.TrimSpace(strArg)
				if trimmed == "" {
					continue
				}
				spec.Args = append(spec.Args, trimmed)
			}
		case []string:
			for _, rawArg := range typedArgs {
				trimmed := strings.TrimSpace(rawArg)
				if trimmed == "" {
					continue
				}
				spec.Args = append(spec.Args, trimmed)
			}
		case string:
			trimmed := strings.TrimSpace(typedArgs)
			if trimmed != "" {
				spec.Args = append(spec.Args, trimmed)
			}
		default:
			cc.logger.Warnw("Ignoring command entry with unsupported args type", "value", argsValue)
			return CommandSpec{}, false
		}
	}

	if len(spec.Args) == 0 {
		return CommandSpec{}, false
	}

	return spec, true
}
