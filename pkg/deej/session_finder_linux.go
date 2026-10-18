//go:build linux
// +build linux

package deej

import (
	"fmt"
	"net"

	"github.com/jfreymuth/pulse/proto"
	"github.com/omriharel/deej/pkg/deej/util"
	"go.uber.org/zap"
)

type paSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	client *proto.Client
	conn   net.Conn

	// dedicated connection for sink-input subscription events
	eventConn net.Conn
	events    chan struct{}
}

const (
	paSubscriptionMaskSinkInput = 0x0004
	paEventFacilityMask         = 0x000F
	paEventFacilitySinkInput    = 0x0002
	paEventTypeMask             = 0x0030
	paEventTypeChange           = 0x0010
)

// SessionEvents returns a channel that receives a signal whenever an audio stream is created or removed.
func (sf *paSessionFinder) SessionEvents() <-chan struct{} {
	return sf.events
}

func (sf *paSessionFinder) startEventSubscription() {
	client, conn, err := proto.Connect("")
	if err != nil {
		sf.logger.Warnw("Failed to open PulseAudio event connection, falling back to polling", "error", err)
		return
	}

	if err := client.Request(&proto.SetClientName{
		Props: proto.PropList{"application.name": proto.PropListString("deej-events")},
	}, &proto.SetClientNameReply{}); err != nil {
		conn.Close()
		sf.logger.Warnw("Failed to name PulseAudio event connection", "error", err)
		return
	}

	// the callback runs on the reader goroutine; never block or issue requests from it
	client.Callback = func(msg interface{}) {
		ev, ok := msg.(*proto.SubscribeEvent)
		if !ok {
			return
		}
		if ev.Event&paEventFacilityMask != paEventFacilitySinkInput {
			return
		}
		// ignore volume/mute "change" events (including our own adjustments)
		if ev.Event&paEventTypeMask == paEventTypeChange {
			return
		}
		select {
		case sf.events <- struct{}{}:
		default:
		}
	}

	if err := client.Request(&proto.Subscribe{Mask: paSubscriptionMaskSinkInput}, nil); err != nil {
		conn.Close()
		sf.logger.Warnw("Failed to subscribe to PulseAudio events", "error", err)
		return
	}

	sf.eventConn = conn
	sf.logger.Debug("Subscribed to PulseAudio sink-input events")
}

func (sf *paSessionFinder) GetForegroundProcessName() (string, error) {
	names, err := util.GetCurrentWindowProcessNames()
	if err != nil || len(names) == 0 {
		return "", err
	}
	return names[0], nil
}

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	client, conn, err := proto.Connect("")
	if err != nil {
		logger.Warnw("Failed to establish PulseAudio connection", "error", err)
		return nil, fmt.Errorf("establish PulseAudio connection: %w", err)
	}

	request := proto.SetClientName{
		Props: proto.PropList{
			"application.name": proto.PropListString("deej"),
		},
	}
	reply := proto.SetClientNameReply{}

	if err := client.Request(&request, &reply); err != nil {
		return nil, err
	}

	sf := &paSessionFinder{
		logger:        logger.Named("session_finder"),
		sessionLogger: logger.Named("sessions"),
		client:        client,
		conn:          conn,
		events:        make(chan struct{}, 1),
	}

	sf.startEventSubscription()

	sf.logger.Debug("Created PA session finder instance")

	return sf, nil
}

func (sf *paSessionFinder) GetAllSessions() ([]Session, error) {
	sessions := []Session{}

	// get the master sink session
	masterSink, err := sf.getMasterSinkSession()
	if err == nil {
		sessions = append(sessions, masterSink)
	} else {
		sf.logger.Warnw("Failed to get master audio sink session", "error", err)
	}

	// get the master source session
	masterSource, err := sf.getMasterSourceSession()
	if err == nil {
		sessions = append(sessions, masterSource)
	} else {
		sf.logger.Warnw("Failed to get master audio source session", "error", err)
	}

	// enumerate sink inputs and add sessions along the way
	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio sessions", "error", err)
		return nil, fmt.Errorf("enumerate audio sessions: %w", err)
	}

	return sessions, nil
}

func (sf *paSessionFinder) Release() error {
	util.CleanupWindowTracker()

	if sf.eventConn != nil {
		_ = sf.eventConn.Close()
	}

	if err := sf.conn.Close(); err != nil {
		sf.logger.Warnw("Failed to close PulseAudio connection", "error", err)
		return fmt.Errorf("close PulseAudio connection: %w", err)
	}

	sf.logger.Debug("Released PA session finder instance")

	return nil
}

func (sf *paSessionFinder) getMasterSinkSession() (Session, error) {
	request := proto.GetSinkInfo{
		SinkIndex: proto.Undefined,
	}
	reply := proto.GetSinkInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master sink info", "error", err)
		return nil, fmt.Errorf("get master sink info: %w", err)
	}

	// create the master sink session
	sink := newMasterSession(sf.sessionLogger, sf.client, reply.SinkIndex, reply.Channels, true)

	return sink, nil
}

func (sf *paSessionFinder) getMasterSourceSession() (Session, error) {
	request := proto.GetSourceInfo{
		SourceIndex: proto.Undefined,
	}
	reply := proto.GetSourceInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master source info", "error", err)
		return nil, fmt.Errorf("get master source info: %w", err)
	}

	// create the master source session
	source := newMasterSession(sf.sessionLogger, sf.client, reply.SourceIndex, reply.Channels, false)

	return source, nil
}

func (sf *paSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	request := proto.GetSinkInputInfoList{}
	reply := proto.GetSinkInputInfoListReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get sink input list", "error", err)
		return fmt.Errorf("get sink input list: %w", err)
	}

	for _, info := range reply {
		name, ok := info.Properties["application.process.binary"]

		if !ok {
			sf.logger.Warnw("Failed to get sink input's process name",
				"sinkInputIndex", info.SinkInputIndex)

			continue
		}

		pidStr := ""
		if pidProp, hasPID := info.Properties["application.process.id"]; hasPID {
			pidStr = pidProp.String()
		}

		processName := util.ResolveProcessBinaryName(name.String(), pidStr)

		// create the deej session object
		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, processName)

		// add it to our slice
		*sessions = append(*sessions, newSession)

	}

	return nil
}

