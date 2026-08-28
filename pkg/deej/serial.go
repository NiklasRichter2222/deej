package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ole "github.com/go-ole/go-ole"
	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	deej   *Deej
	logger *zap.SugaredLogger

	stopChannel     chan struct{}
	stopOnce        sync.Once
	startOnce       sync.Once
	reconnectSignal chan struct{}

	connMu      sync.Mutex
	connected   bool
	connOptions serial.OpenOptions
	conn        io.ReadWriteCloser

	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent

	writeMu sync.Mutex

	lastSentSliderPositions   map[int]float32
	lastSentSliderPositionsMu sync.Mutex

	lastSentSliderMutes   map[int]bool
	lastSentSliderMutesMu sync.Mutex

	// suppress incoming slider move events until this time. used to avoid
	// echo/feedback loops when we send initial display values to the controller
	suppressSliderEventsUntil   time.Time
	suppressSliderEventsUntilMu sync.Mutex
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                    deej,
		logger:                  logger,
		stopChannel:             make(chan struct{}),
		reconnectSignal:         make(chan struct{}, 1),
		connected:               false,
		conn:                    nil,
		sliderMoveConsumers:     []chan SliderMoveEvent{},
		lastSentSliderPositions: make(map[int]float32),
		lastSentSliderMutes:     make(map[int]bool),
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start starts the background serial connection supervisor
func (sio *SerialIO) Start() error {
	sio.startOnce.Do(func() {
		go sio.manageConnection()
	})
	return nil
}

// IsConnected returns whether the serial connection is currently active
func (sio *SerialIO) IsConnected() bool {
	sio.connMu.Lock()
	defer sio.connMu.Unlock()
	return sio.connected && sio.conn != nil
}

func (sio *SerialIO) triggerReconnect() {
	select {
	case sio.reconnectSignal <- struct{}{}:
	default:
	}
}

// manageConnection runs the supervisor loop that connects and auto-reconnects
func (sio *SerialIO) manageConnection() {
	minimumReadSize := 0
	if util.Linux() {
		minimumReadSize = 1
	}

	for {
		select {
		case <-sio.stopChannel:
			sio.closeConnection()
			return
		default:
		}

		portName := sio.deej.config.ConnectionInfo.COMPort
		baudRate := uint(sio.deej.config.ConnectionInfo.BaudRate)

		sio.connOptions = serial.OpenOptions{
			PortName:        portName,
			BaudRate:        baudRate,
			DataBits:        8,
			StopBits:        1,
			MinimumReadSize: uint(minimumReadSize),
		}

		conn, err := serial.Open(sio.connOptions)
		if err != nil {
			sio.logger.Debugw("Waiting to connect to serial port",
				"comPort", portName,
				"baudRate", baudRate,
				"error", err)

			select {
			case <-sio.stopChannel:
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		namedLogger := sio.logger.Named(strings.ToLower(portName))
		namedLogger.Infow("Connected to serial device", "comPort", portName, "baudRate", baudRate)

		sio.connMu.Lock()
		sio.conn = conn
		sio.connected = true
		sio.connMu.Unlock()

		sio.resetSliderDisplayCache()

		if sio.deej.sessions != nil {
			sio.deej.sessions.refreshSessions(true)
		}

		// Run active connection session until disconnection, config change, or stop
		sio.runSession(namedLogger, conn)

		// Close connection cleanly after session termination
		sio.closeConnection()

		select {
		case <-sio.stopChannel:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (sio *SerialIO) runSession(logger *zap.SugaredLogger, conn io.ReadWriteCloser) {
	runtime.LockOSThread()
	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer runtime.UnlockOSThread()

	connReader := bufio.NewReader(conn)
	lineChannel := make(chan string, 32)
	readErrChannel := make(chan error, 1)
	sessionDone := make(chan struct{})
	var sessionDoneOnce sync.Once

	closeSession := func() {
		sessionDoneOnce.Do(func() {
			close(sessionDone)
		})
	}
	defer closeSession()

	// Line reader goroutine
	go func() {
		for {
			line, err := connReader.ReadString('\n')
			if err != nil {
				select {
				case readErrChannel <- err:
				default:
				}
				closeSession()
				return
			}

			select {
			case lineChannel <- line:
			case <-sessionDone:
				return
			}
		}
	}()

	// Heartbeat ticker (sends HB every 1000ms to keep Arduino connection timer alive)
	heartbeatTicker := time.NewTicker(1000 * time.Millisecond)
	defer heartbeatTicker.Stop()

	firstLineReceived := false

	// Initial configuration and volumes sync shortly after connection
	go func() {
		time.Sleep(150 * time.Millisecond)
		if sio.IsConnected() {
			_ = sio.sendLightingConfiguration(logger)
			_ = sio.sendInitialSliderVolumes(logger)
		}
	}()

	for {
		select {
		case <-sio.stopChannel:
			return

		case <-sio.reconnectSignal:
			logger.Info("Reconnection requested, recycling connection")
			return

		case err := <-readErrChannel:
			logger.Warnw("Serial read error / disconnected", "error", err)
			return

		case <-heartbeatTicker.C:
			if sio.IsConnected() {
				_ = sio.writeSerialLine("HB")
			}

		case line := <-lineChannel:
			if !firstLineReceived {
				firstLineReceived = true

				if err := sio.sendLightingConfiguration(logger); err != nil {
					logger.Warnw("Failed to send lighting configuration", "error", err)
				}

				if err := sio.sendInitialSliderVolumes(logger); err != nil {
					logger.Warnw("Failed to send initial slider volumes", "error", err)
				}
			}

			sio.handleLine(logger, line)
		}
	}
}

// Stop signals us to shut down our serial connection supervisor
func (sio *SerialIO) Stop() {
	sio.stopOnce.Do(func() {
		sio.logger.Debug("Shutting down serial connection supervisor")
		close(sio.stopChannel)
		sio.closeConnection()
	})
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for {
			select {
			case <-configReloadedChannel:
				// make any config reload unset our slider number to ensure process volumes are being re-set
				go func() {
					<-time.After(stopDelay)
					sio.lastKnownNumSliders = 0
				}()

				sio.connMu.Lock()
				currentPort := sio.connOptions.PortName
				currentBaud := sio.connOptions.BaudRate
				sio.connMu.Unlock()

				// if connection params have changed, trigger reconnect
				if sio.deej.config.ConnectionInfo.COMPort != currentPort ||
					uint(sio.deej.config.ConnectionInfo.BaudRate) != currentBaud {

					sio.logger.Info("Detected change in connection parameters, triggering reconnect")
					sio.triggerReconnect()
				} else if sio.IsConnected() {
					go func() {
						<-time.After(stopDelay)
						if !sio.IsConnected() {
							return
						}

						if err := sio.sendLightingConfiguration(sio.logger); err != nil {
							sio.logger.Warnw("Failed to send lighting configuration after reload", "error", err)
						}
					}()
				}
			}
		}
	}()
}

func (sio *SerialIO) closeConnection() {
	sio.connMu.Lock()
	defer sio.connMu.Unlock()

	sio.writeMu.Lock()
	defer sio.writeMu.Unlock()

	if sio.conn != nil {
		if err := sio.conn.Close(); err != nil {
			sio.logger.Debugw("Serial connection closed with error", "error", err)
		} else {
			sio.logger.Debug("Serial connection closed")
		}
		sio.conn = nil
	}

	sio.connected = false
	sio.resetSliderDisplayCache()
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {
	sanitized := strings.TrimRight(line, "\r\n")

	if sanitized == "" {
		return
	}

	if sio.tryHandleMuteCommand(logger, sanitized) {
		return
	}

	// fast validation: check if the string contains only digits and pipes, and doesn't start/end with pipe
	if len(sanitized) == 0 || sanitized[0] == '|' || sanitized[len(sanitized)-1] == '|' {
		return
	}
	for i := 0; i < len(sanitized); i++ {
		c := sanitized[i]
		if (c < '0' || c > '9') && c != '|' {
			return
		}
	}

	// split on pipe (|), this gives a slice of numerical strings between "0" and "1023"
	splitLine := strings.Split(sanitized, "|")
	numSliders := len(splitLine)

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, _ := strconv.Atoi(stringValue)

		// turns out serial lines can occasionally come out dirty; reject any out-of-range value
		if number < 0 || number > 1023 {
			sio.logger.Debugw("Got malformed line from serial, ignoring", "line", sanitized)
			return
		}

		// map the value from raw to a "dirty" float between 0 and 1 (e.g. 0.15451...)
		dirtyFloat := float32(number) / 1023.0

		// normalize it to an actual volume scalar between 0.0 and 1.0 with 2 points of precision
		normalizedScalar := util.NormalizeScalar(dirtyFloat)

		// if sliders are inverted, take the complement of 1.0
		if sio.deej.config.InvertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {

			// if it does, update the saved value and create a move event
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar

			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	// deliver move events if there are any, towards all potential consumers
	if len(moveEvents) > 0 {
		// check if we're currently suppressing incoming slider events (e.g. during startup sync)
		sio.suppressSliderEventsUntilMu.Lock()
		suppressUntil := sio.suppressSliderEventsUntil
		sio.suppressSliderEventsUntilMu.Unlock()

		if time.Now().Before(suppressUntil) {
			if sio.deej.Verbose() {
				logger.Debugw("Ignoring incoming slider events due to startup sync", "count", len(moveEvents))
			}
			return
		}

		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}

func (sio *SerialIO) tryHandleMuteCommand(logger *zap.SugaredLogger, line string) bool {
	if len(line) < 3 {
		return false
	}

	if (line[0] != 'M' && line[0] != 'm') || line[1] != ':' {
		return false
	}

	payload := strings.TrimSpace(line[2:])
	if payload == "" {
		if sio.deej.Verbose() {
			logger.Debugw("Ignoring mute command with empty payload", "line", line)
		}
		return true
	}

	index, err := strconv.Atoi(payload)
	if err != nil {
		if sio.deej.Verbose() {
			logger.Debugw("Ignoring mute command with non-numeric index", "payload", payload)
		}
		return true
	}

	if err := sio.deej.sessions.ToggleSliderMute(index); err != nil {
		logger.Warnw("Failed to toggle slider mute", "slider", index, "error", err)
	}

	return true
}

func (sio *SerialIO) sendLightingConfiguration(logger *zap.SugaredLogger) error {
	if !sio.deej.config.SendOnStartup {
		return nil
	}

	if !sio.IsConnected() {
		return errors.New("serial: connection not established")
	}

	// Send default brightness
	_ = sio.writeSerialLine(fmt.Sprintf("BR:%.2f", sio.deej.config.DefaultBrightness))
	time.Sleep(10 * time.Millisecond)

	background := strings.TrimSpace(sio.deej.config.BackgroundLighting)
	if background != "" {
		if err := sio.writeSerialLine(fmt.Sprintf("B:%s", background)); err != nil {
			return fmt.Errorf("send background lighting: %w", err)
		}
		time.Sleep(10 * time.Millisecond)

		if sio.deej.Verbose() {
			logger.Debugw("Sent background lighting", "value", background)
		}
	}

	// Send page button colors
	leftBtnColor := sio.deej.config.PageButtonLeftColor
	if leftBtnColor == "" {
		leftBtnColor = "#ffffff"
	}
	leftBtnOffColor := sio.deej.config.PageButtonLeftOffColor
	if leftBtnOffColor == "" {
		leftBtnOffColor = "#000000"
	}

	rightBtnColor := sio.deej.config.PageButtonRightColor
	if rightBtnColor == "" {
		rightBtnColor = "#ffffff"
	}
	rightBtnOffColor := sio.deej.config.PageButtonRightOffColor
	if rightBtnOffColor == "" {
		rightBtnOffColor = "#000000"
	}

	_ = sio.writeSerialLine(fmt.Sprintf("CP:0:%s:%s", leftBtnColor, leftBtnOffColor))
	time.Sleep(10 * time.Millisecond)
	_ = sio.writeSerialLine(fmt.Sprintf("CP:1:%s:%s", rightBtnColor, rightBtnOffColor))
	time.Sleep(10 * time.Millisecond)

	// Send slider colors
	indices := make([]int, 0, len(sio.deej.config.ColorMapping))
	for idx := range sio.deej.config.ColorMapping {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		entry := sio.deej.config.ColorMapping[idx]
		zero := strings.TrimSpace(entry.Zero)
		full := strings.TrimSpace(entry.Full)
		if zero != "" && full != "" {
			_ = sio.writeSerialLine(fmt.Sprintf("C:%d:%s:%s", idx, zero, full))
			time.Sleep(10 * time.Millisecond)
		}
	}

	return nil
}

// sendInitialSliderVolumes pushes the current session volumes to the controller for startup sync.
func (sio *SerialIO) sendInitialSliderVolumes(logger *zap.SugaredLogger) error {
	if !sio.deej.config.SendOnStartup {
		return nil
	}

	indices := []int{}

	sio.deej.config.SliderMapping.iterate(func(sliderIdx int, _ []string) {
		indices = append(indices, sliderIdx)
	})

	if len(indices) == 0 {
		return nil
	}

	sort.Ints(indices)

	// suppress incoming slider move events for a short window so the controller's
	// initial echo doesn't cause deej to accidentally apply the same values to
	// system/app volumes.
	const startupSuppress = 700 * time.Millisecond
	sio.suppressSliderEventsUntilMu.Lock()
	sio.suppressSliderEventsUntil = time.Now().Add(startupSuppress)
	sio.suppressSliderEventsUntilMu.Unlock()

	for _, idx := range indices {
		volume, ok := sio.deej.sessions.sliderVolume(idx)
		if ok {
			if err := sio.SendSliderDisplayValue(idx, volume); err != nil {
				return fmt.Errorf("send initial volume for slider %d: %w", idx, err)
			}
			time.Sleep(5 * time.Millisecond)
		}

		muted, ok := sio.deej.sessions.sliderMute(idx)
		if ok {
			if err := sio.SendSliderMute(idx, muted); err != nil {
				return fmt.Errorf("send initial mute for slider %d: %w", idx, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	return nil
}

// SendSliderDisplayValue sends a display update for a slider, caching the last transmitted value.
func (sio *SerialIO) SendSliderDisplayValue(sliderIdx int, percent float32) error {
	if !sio.IsConnected() {
		return nil
	}

	if percent < 0 {
		percent = 0
	} else if percent > 1 {
		percent = 1
	}

	position := percent
	if sio.deej.config.InvertSliders {
		position = 1 - position
	}

	position = util.NormalizeScalar(position)

	sio.lastSentSliderPositionsMu.Lock()
	last, ok := sio.lastSentSliderPositions[sliderIdx]
	sio.lastSentSliderPositionsMu.Unlock()

	if ok && last == position {
		return nil
	}

	payload := fmt.Sprintf("V:%d:%.3f", sliderIdx, position)
	if err := sio.writeSerialLine(payload); err != nil {
		return err
	}

	sio.lastSentSliderPositionsMu.Lock()
	sio.lastSentSliderPositions[sliderIdx] = position
	sio.lastSentSliderPositionsMu.Unlock()

	if sio.deej.Verbose() {
		sio.logger.Debugw("Sent slider display update", "slider", sliderIdx, "percent", percent, "position", position)
	}

	return nil
}

// SendSliderMute sends a mute update for a slider, caching the last transmitted value.
func (sio *SerialIO) SendSliderMute(sliderIdx int, muted bool) error {
	if !sio.IsConnected() {
		return nil
	}

	sio.lastSentSliderMutesMu.Lock()
	last, ok := sio.lastSentSliderMutes[sliderIdx]
	sio.lastSentSliderMutesMu.Unlock()

	if ok && last == muted {
		return nil
	}

	mutedInt := 0
	if muted {
		mutedInt = 1
	}

	payload := fmt.Sprintf("M:%d:%d", sliderIdx, mutedInt)
	if err := sio.writeSerialLine(payload); err != nil {
		return err
	}

	sio.lastSentSliderMutesMu.Lock()
	sio.lastSentSliderMutes[sliderIdx] = muted
	sio.lastSentSliderMutesMu.Unlock()

	if sio.deej.Verbose() {
		sio.logger.Debugw("Sent slider mute update", "slider", sliderIdx, "muted", muted)
	}

	return nil
}

func (sio *SerialIO) resetSliderDisplayCache() {
	sio.lastSentSliderPositionsMu.Lock()
	sio.lastSentSliderPositions = make(map[int]float32)
	sio.lastSentSliderPositionsMu.Unlock()

	sio.lastSentSliderMutesMu.Lock()
	sio.lastSentSliderMutes = make(map[int]bool)
	sio.lastSentSliderMutesMu.Unlock()
}

func (sio *SerialIO) writeSerialLine(payload string) error {
	sio.writeMu.Lock()
	defer sio.writeMu.Unlock()

	sio.connMu.Lock()
	conn := sio.conn
	connected := sio.connected
	sio.connMu.Unlock()

	if conn == nil || !connected {
		return errors.New("serial: connection not active")
	}

	if !strings.HasSuffix(payload, "\r\n") {
		payload += "\r\n"
	}

	if sio.deej.Verbose() {
		sio.logger.Debugw("writing to serial", "payload", payload)
	}

	_, err := conn.Write([]byte(payload))
	if err != nil {
		sio.triggerReconnect()
		return err
	}
	return nil
}
