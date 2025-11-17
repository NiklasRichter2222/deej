package deej

import (
	"errors"
	"fmt"
	"strings"

	ole "github.com/go-ole/go-ole"
	ps "github.com/mitchellh/go-ps"
	wca "github.com/moutend/go-wca"
	"go.uber.org/zap"
)

var errNoSuchProcess = errors.New("No such process")
var errRefreshSessions = errors.New("Trigger session refresh")

type wcaSession struct {
	baseSession

	pid         uint32
	processName string

	control *wca.IAudioSessionControl2
	volume  *wca.ISimpleAudioVolume

	eventCtx *ole.GUID
}

type masterSession struct {
	baseSession

	volume *wca.IAudioEndpointVolume

	eventCtx *ole.GUID

	stale bool // when set to true, we should refresh sessions on the next call to SetVolume
}

func newWCASession(
	logger *zap.SugaredLogger,
	control *wca.IAudioSessionControl2,
	volume *wca.ISimpleAudioVolume,
	pid uint32,
	eventCtx *ole.GUID,
) (*wcaSession, error) {

	s := &wcaSession{
		control:  control,
		volume:   volume,
		pid:      pid,
		eventCtx: eventCtx,
	}

	// special treatment for system sounds session
	if pid == 0 {
		s.system = true
		s.name = systemSessionName
		s.humanReadableDesc = "system sounds"
	} else {

		// find our session's process name
		process, err := ps.FindProcess(int(pid))
		if err != nil {
			logger.Warnw("Failed to find process name by ID", "pid", pid, "error", err)
			defer s.Release()

			return nil, fmt.Errorf("find process name by pid: %w", err)
		}

		// this PID may be invalid - this means the process has already been
		// closed and we shouldn't create a session for it.
		if process == nil {
			logger.Debugw("Process already exited, not creating audio session", "pid", pid)
			return nil, errNoSuchProcess
		}

		s.processName = process.Executable()
		s.name = s.processName
		s.humanReadableDesc = fmt.Sprintf("%s (pid %d)", s.processName, s.pid)
	}

	// use a self-identifying session name e.g. deej.sessions.chrome
	s.logger = logger.Named(strings.TrimSuffix(s.Key(), ".exe"))
	s.logger.Debugw(sessionCreationLogMessage, "session", s)

	return s, nil
}

func newMasterSession(
	logger *zap.SugaredLogger,
	volume *wca.IAudioEndpointVolume,
	eventCtx *ole.GUID,
	key string,
	loggerKey string,
) (*masterSession, error) {

	s := &masterSession{
		volume:   volume,
		eventCtx: eventCtx,
	}

	s.logger = logger.Named(loggerKey)
	s.master = true
	s.name = key
	s.humanReadableDesc = key

	s.logger.Debugw(sessionCreationLogMessage, "session", s)

	return s, nil
}

func (s *wcaSession) GetVolume() float32 {
	var level float32

	if err := s.volume.GetMasterVolume(&level); err != nil {
		s.logger.Warnw("Failed to get session volume", "error", err)
	}

	return level
}

func (s *wcaSession) SetVolume(v float32) error {
	if err := s.volume.SetMasterVolume(v, s.eventCtx); err != nil {
		s.logger.Warnw("Failed to set session volume", "error", err)
		return fmt.Errorf("adjust session volume: %w", err)
	}

	// mitigate expired sessions by checking the state whenever we change volumes
	var state uint32

	if err := s.control.GetState(&state); err != nil {
		s.logger.Warnw("Failed to get session state while setting volume", "error", err)
		return fmt.Errorf("get session state: %w", err)
	}

	if state == wca.AudioSessionStateExpired {
		s.logger.Warnw("Audio session expired, triggering session refresh")
		return errRefreshSessions
	}

	s.logger.Debugw("Adjusting session volume", "to", fmt.Sprintf("%.2f", v))

	return nil
}

func (s *wcaSession) Release() {
	s.logger.Debug("Releasing audio session")

	s.volume.Release()
	s.control.Release()
}

func (s *wcaSession) String() string {
	return fmt.Sprintf(sessionStringFormat, s.humanReadableDesc, s.GetVolume())
}

func (s *masterSession) GetVolume() float32 {
	var level float32

	if err := s.volume.GetMasterVolumeLevelScalar(&level); err != nil {
		s.logger.Warnw("Failed to get session volume", "error", err)
	}

	return level
}

func (s *masterSession) SetVolume(v float32) error {
	if s.stale {
		s.logger.Warnw("Session expired because default device has changed, triggering session refresh")
		return errRefreshSessions
	}

	if err := s.volume.SetMasterVolumeLevelScalar(v, s.eventCtx); err != nil {
		s.logger.Warnw("Failed to set session volume",
			"error", err,
			"volume", v)

		return fmt.Errorf("adjust session volume: %w", err)
	}

	s.logger.Debugw("Adjusting session volume", "to", fmt.Sprintf("%.2f", v))

	return nil
}

func (s *masterSession) Release() {
	s.logger.Debug("Releasing audio session")

	s.volume.Release()
}

func (s *masterSession) String() string {
	return fmt.Sprintf(sessionStringFormat, s.humanReadableDesc, s.GetVolume())
}

func (s *masterSession) markAsStale() {
	s.stale = true
}

// ReadMasterVolume returns the current master (default output device) volume as a scalar between 0.0 and 1.0.
// It enumerates audio sessions and returns the first session whose key equals the master session name.
func ReadMasterVolume() (float32, error) {
	logger := zap.NewNop().Sugar()
	sf, err := newSessionFinder(logger)
	if err != nil {
		return 0, fmt.Errorf("create session finder: %w", err)
	}
	defer sf.Release()

	sessions, err := sf.GetAllSessions()
	if err != nil {
		return 0, fmt.Errorf("get all sessions: %w", err)
	}

	for _, s := range sessions {
		if s.Key() == masterSessionName {
			return s.GetVolume(), nil
		}
	}

	return 0, fmt.Errorf("master session not found")
}

// ReadAppVolumeByName looks up an application's audio session by process name (case-insensitive)
// and returns its current volume as a scalar between 0.0 and 1.0. The provided name may include
// or omit the ".exe" suffix; the function will check both variants.
func ReadAppVolumeByName(name string) (float32, error) {
	logger := zap.NewNop().Sugar()
	sf, err := newSessionFinder(logger)
	if err != nil {
		return 0, fmt.Errorf("create session finder: %w", err)
	}
	defer sf.Release()

	sessions, err := sf.GetAllSessions()
	if err != nil {
		return 0, fmt.Errorf("get all sessions: %w", err)
	}

	target := strings.ToLower(name)
	alt := target
	if !strings.HasSuffix(target, ".exe") {
		alt = target + ".exe"
	}

	for _, s := range sessions {
		k := s.Key()
		if k == target || k == alt {
			return s.GetVolume(), nil
		}
	}

	return 0, fmt.Errorf("no audio session found for process '%s'", name)
}

// ReadAppVolumeByPID looks up an application's audio session by process ID and returns its current
// volume as a scalar between 0.0 and 1.0. This is Windows-specific and relies on the concrete
// `wcaSession` type, which exposes the PID.
func ReadAppVolumeByPID(pid int) (float32, error) {
	logger := zap.NewNop().Sugar()
	sf, err := newSessionFinder(logger)
	if err != nil {
		return 0, fmt.Errorf("create session finder: %w", err)
	}
	defer sf.Release()

	sessions, err := sf.GetAllSessions()
	if err != nil {
		return 0, fmt.Errorf("get all sessions: %w", err)
	}

	for _, s := range sessions {
		if ws, ok := s.(*wcaSession); ok {
			if int(ws.pid) == pid {
				return ws.GetVolume(), nil
			}
		}
	}

	return 0, fmt.Errorf("no audio session found for pid %d", pid)
}
