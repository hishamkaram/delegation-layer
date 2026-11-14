package browser

import (
	"sync"

	"github.com/godbus/dbus/v5"
)

const responseSignal = "org.freedesktop.portal.Request.Response"

// portalSignals retains only the first exact response. Godbus's default handler
// starts a goroutine per backpressured signal; this bounded handler starts none.
// Conn.Close owns Terminate and channel closure.
type portalSignals struct {
	mu       sync.Mutex
	owner    string
	path     dbus.ObjectPath
	closed   bool
	response chan *dbus.Signal
}

func newPortalSignals() *portalSignals {
	return &portalSignals{response: make(chan *dbus.Signal, 1)}
}

func (s *portalSignals) selectRequest(owner string, path dbus.ObjectPath) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner, s.path = owner, path
}

func (s *portalSignals) DeliverSignal(_ string, _ string, signal *dbus.Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || signal == nil || signal.Name != responseSignal || signal.Path != s.path || signal.Sender != s.owner || s.owner == "" {
		return
	}
	select {
	case s.response <- signal:
	default:
	}
}

func (s *portalSignals) Terminate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.response)
	}
}

func responseResult(signal *dbus.Signal) (bool, error) {
	if signal == nil || len(signal.Body) != 2 {
		return false, errActivation
	}
	code, valid := signal.Body[0].(uint32)
	if !valid || code > 2 {
		return false, errActivation
	}
	if _, valid = signal.Body[1].(map[string]dbus.Variant); !valid {
		return false, errActivation
	}
	if code != 0 {
		return true, errActivation
	}
	return true, nil
}
