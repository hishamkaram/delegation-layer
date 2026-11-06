package infrastructure

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

const (
	ownerFrameReadTimeout = 5 * time.Second
	ownerPreauthLimit     = 16
	ownerOperationLimit   = 16
	ownerControlLimit     = 4
	ownerBusyWriteTimeout = time.Second
)

// ownerServer borrows a successfully born entry. It owns connection/work admission,
// the immutable initial outcome and delivery, and the daemon Wait collection. Its caller must drain it before
// closing the entry; transport/accept faults never close resident resources or
// stop daemons.
type ownerServer struct {
	entry    *ownedResidentEntry
	host     EntryHost
	listener *net.UnixListener
	journal  *intentJournal
	waits    *daemonWaitOwner
	// Fixed native I/O, private for deterministic resource-fault tests; no app override.
	closeConnection   func(*net.UnixConn) error
	wakeConnection    func(*net.UnixConn, time.Time) error
	writeReply        func(*net.UnixConn, ownerReply) error
	mu                sync.Mutex
	closing           bool
	serving           bool
	connections       map[*net.UnixConn]ownerConnectionClass
	drained           chan struct{}
	drainedClosed     bool
	initial           int
	initialDone       chan struct{}
	initialCompleted  bool
	initialOutcome    PreparationOutcome
	initialError      error
	terminalHandoff   bool
	admissionStopped  chan struct{}
	initialDelivered  chan struct{}
	deliveryCompleted bool
	cleanupFailure    error
	requestDiagnostic error
}

func newOwnerServer(ctx context.Context, entry *ownedResidentEntry, host EntryHost) (*ownerServer, error) {
	if ctx == nil || entry == nil || host == nil {
		return nil, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed || entry.endpoint == nil || entry.journal == nil || entry.grant == nil {
		return nil, os.ErrClosed
	}
	if err := entry.operation.Validate(); err != nil {
		return nil, err
	}
	if err := entry.validateResident(ctx); err != nil {
		return nil, err
	}
	intent := entry.operation.Intent()
	origin, err := NewOrigin(intent.Inputs.Root, intent.Inputs.RootID)
	if err != nil {
		return nil, err
	}
	waits, err := newDaemonWaitOwner(origin)
	if err != nil {
		return nil, err
	}
	return &ownerServer{
		entry: entry, host: host, listener: entry.endpoint.listener,
		journal: entry.journal, waits: waits,
		closeConnection: (*net.UnixConn).Close,
		wakeConnection:  (*net.UnixConn).SetDeadline,
		writeReply:      func(connection *net.UnixConn, reply ownerReply) error { return writeOwnerFrame(connection, reply) },
		initialDone:     make(chan struct{}), initialDelivered: make(chan struct{}), admissionStopped: make(chan struct{}),
		connections: make(map[*net.UnixConn]ownerConnectionClass),
		drained:     make(chan struct{}),
	}, nil
}

// prepareInitial retains the original startup deadline; live owner RPCs use
// their own finite request deadlines and never renew this initial preparation.
func (s *ownerServer) prepareInitial(ctx context.Context) (disposition Disposition, resultErr error) {
	if s == nil {
		return disposition, os.ErrInvalid
	}
	var outcome PreparationOutcome
	dispatched := false
	defer func() {
		if !dispatched && outcome == (PreparationOutcome{}) {
			outcome = s.initialFailure(resultErr)
		}
		s.completeInitial(outcome, resultErr)
	}()
	if ctx == nil {
		return disposition, os.ErrInvalid
	}
	deadline, err := canonicalStartupTime(s.entry.operation.Intent().Deadline)
	if err != nil {
		return disposition, err
	}
	operation, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err = operation.Err(); err != nil {
		return disposition, err
	}
	if err = s.registerInitial(); err != nil {
		return disposition, err
	}
	defer s.finishInitial()
	selected, reference, err := resolveSelectedContext(operation, s.journal, s.entry.operation.defaultContextReference(), LaunchContextReference{})
	if err != nil {
		return disposition, err
	}
	initial := s.entry.operation.Intent().Inputs.Initial
	startup, err := ReferenceStartup(s.entry.operation.Intent())
	if err != nil {
		return disposition, err
	}
	work := acceptedOwnerWork{operation: s.entry.operation, selected: selected, selectedReference: reference, request: ownerRequest{Owner: startup, Action: preparationAction(initial), Source: initial.Source}}
	dispatched = true
	outcome, resultErr = s.executePreparation(operation, work)
	return outcome.Disposition, resultErr
}

// serve owns each accepted connection until its one request has finished. A
// failure returns to the serving caller with the borrowed entry still owned.
func (s *ownerServer) serve(ctx context.Context) (resultErr error) {
	if s == nil || ctx == nil {
		return os.ErrInvalid
	}
	if err := s.beginServing(); err != nil {
		return err
	}
	defer s.finishServing()
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); s.stopAccepting() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	for {
		connection, err := s.listener.AcceptUnix()
		if err != nil {
			closing := s.isClosing()
			s.stopAccepting()
			if closing && (errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, net.ErrClosed)) {
				return ctx.Err()
			}
			s.connectionFault(err)
			return ownerServerError{err}
		}
		if !s.registerConnection(connection) {
			if err = connection.Close(); err != nil {
				s.cleanupFault(err)
				s.stopAccepting()
				return ownerServerError{err}
			}
			continue
		}
		go s.handleConnection(context.WithoutCancel(ctx), connection)
	}
}

func (s *ownerServer) beginServing() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serving || s.closing {
		return os.ErrClosed
	}
	s.serving = true
	return nil
}

func (s *ownerServer) finishServing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serving = false
	s.signalDrained()
}

func (s *ownerServer) registerConnection(connection *net.UnixConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.connectionCount(ownerConnectionPreauth) >= ownerPreauthLimit {
		return false
	}
	s.connections[connection] = ownerConnectionPreauth
	return true
}

func (s *ownerServer) admitConnection(connection *net.UnixConn, action ownerAction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return os.ErrClosed
	}
	if _, present := s.connections[connection]; !present {
		return os.ErrClosed
	}
	class, limit := ownerConnectionOperation, ownerOperationLimit
	if action == ownerStatus {
		class, limit = ownerConnectionControl, ownerControlLimit
	}
	count := s.connectionCount(class)
	if class == ownerConnectionOperation {
		count += s.initial
	}
	if count >= limit {
		return errOwnerWorkBusy
	}
	s.connections[connection] = class
	return nil
}

// finishConnection keeps native Close and map removal in one mutex phase.
// Every registered member remains open whenever another owner can inspect it.
func (s *ownerServer) finishConnection(connection *net.UnixConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, present := s.connections[connection]; !present {
		return
	}
	closeErr := s.closeConnection(connection)
	delete(s.connections, connection)
	if closeErr != nil {
		s.recordCleanupFault(closeErr)
	}
	s.signalDrained()
}

func (s *ownerServer) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// stopAccepting never closes the borrowed listener. Before-admission reads are
// interrupted; accepted work retains its original deadline and is not canceled.
func (s *ownerServer) stopAccepting() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAcceptingHeld()
}

// Caller holds the admission mutex so a successful stop cannot race a new slot.
func (s *ownerServer) stopAcceptingHeld() {
	s.closing = true
	select {
	case <-s.admissionStopped:
	default:
		close(s.admissionStopped)
	}
	if err := s.listener.SetDeadline(time.Now()); err != nil {
		s.recordCleanupFault(err)
	}
	for connection, class := range s.connections {
		if class == ownerConnectionPreauth {
			if err := s.wakeConnection(connection, time.Now()); err != nil {
				s.recordCleanupFault(err)
			}
		}
	}
	s.signalDrained()
}

// stopAcceptAndDrain settles callbacks, not their retained canonical workers.
// The runtime keeps Wait registration open until those workers also complete.
// A timeout retains borrowers; it grants no entry or daemon closure.
func (s *ownerServer) stopAcceptAndDrain(ctx context.Context) error {
	if s == nil || ctx == nil {
		return os.ErrInvalid
	}
	s.stopAccepting()
	select {
	case <-s.drained:
		return s.fault()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Signal only under mu, after both admission has closed and all work stopped.
func (s *ownerServer) signalDrained() {
	if s.closing && !s.serving && len(s.connections) == 0 && s.initial == 0 && !s.drainedClosed {
		close(s.drained)
		s.drainedClosed = true
	}
}

type ownerServerError struct{ cause error }

func (ownerServerError) Error() string   { return "infrastructure owner server failed" }
func (e ownerServerError) Unwrap() error { return e.cause }

func (s *ownerServer) registerInitial() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return os.ErrClosed
	}
	if s.connectionCount(ownerConnectionOperation) >= ownerOperationLimit {
		return errOwnerWorkBusy
	}
	s.initial++
	return nil
}

func (s *ownerServer) finishInitial() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initial--
	s.signalDrained()
}

// The mutex owner retains only the first bounded diagnostic, never a growing
// list of attacker-driven connection faults.
func (s *ownerServer) recordCleanupFault(err error) {
	if s.cleanupFailure == nil {
		s.cleanupFailure = ownerServerError{err}
	}
}

func (s *ownerServer) fault() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupFailure
}

// The connection map is the sole accounting owner. Counts are derived under mu
// so pre-auth, operational and reserved control actors never share a cap.
type ownerConnectionClass uint8

const (
	ownerConnectionPreauth ownerConnectionClass = iota
	ownerConnectionOperation
	ownerConnectionControl
)

var errOwnerWorkBusy = errors.New("infrastructure owner operation capacity unavailable")

func (s *ownerServer) connectionCount(class ownerConnectionClass) int {
	count := 0
	for _, current := range s.connections {
		if current == class {
			count++
		}
	}
	return count
}

// diagnostic is a bounded observation for the private parent. It is not an
// unresolved resource fault and must never be used as closure authority.
func (s *ownerServer) diagnostic() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requestDiagnostic
}

func (s *ownerServer) cleanupFault(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCleanupFault(err)
}
