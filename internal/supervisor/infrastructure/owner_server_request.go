package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type acceptedOwnerWork struct {
	request           ownerRequest
	operation         Operation
	selected          LaunchContext
	selectedReference LaunchContextReference
}

func (s *ownerServer) handleConnection(parent context.Context, connection *net.UnixConn) {
	defer s.finishConnection(connection)
	if err := connection.SetDeadline(time.Now().Add(ownerFrameReadTimeout)); err != nil {
		s.connectionFault(err)
		return
	}
	request, stop, err := readOwnerControlFrame(bufio.NewReader(connection))
	if err != nil {
		if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return
		}
		s.connectionFault(err)
		return
	}
	if stop.StopProtocol != 0 {
		s.handleStopConnection(parent, connection, stop)
		return
	}
	// This request owns its finite wire deadline. Socket loss does not cancel
	// accepted work; no transport context is retained by the canonical operation.
	ctx, cancel, err := ownerContactContext(parent, request)
	if err != nil {
		s.connectionFault(err)
		return
	}
	defer cancel()
	deadline, _ := ctx.Deadline() // ownerContactContext requires a finite deadline.
	if err = s.setRequestDeadline(connection, deadline); err != nil {
		s.connectionFault(err)
		return
	}
	work, err := s.acceptWork(ctx, connection, request)
	if err != nil {
		s.refuseOwnerWork(ctx, connection, request, err)
		return
	}

	s.respondToWork(ctx, connection, work)
}

func (s *ownerServer) respondToWork(ctx context.Context, connection *net.UnixConn, work acceptedOwnerWork) {
	reply, initialResult := s.executeWork(ctx, work)
	if err := s.writeReply(connection, reply); err != nil {
		s.connectionFault(err)
	} else if initialResult {
		s.deliverInitialResult()
	}
}

// Cleanup-owning error wrappers retain their actual owner graph. Once one
// appears, close admission atomically before releasing its operation slot.
// At most the already-admitted operations can add further retained failures.
// Ordinary diagnostics remain bounded independently and never prove closure.
func (s *ownerServer) connectionFault(err error) {
	retained := retainsResidentCleanup(err)
	s.mu.Lock()
	if s.requestDiagnostic == nil {
		s.requestDiagnostic = ownerServerError{err}
	}
	if retained {
		s.closing = true
		s.cleanupFailure = errors.Join(s.cleanupFailure, ownerServerError{err})
	}
	s.mu.Unlock()
	if retained {
		s.stopAccepting()
	}
}

func retainsResidentCleanup(err error) bool {
	return task.AnyErrorNode(err, func(node error) bool {
		_, ok := node.(interface{ RetainsResidentCleanup() })
		return ok
	})
}

func (s *ownerServer) acceptWork(ctx context.Context, connection *net.UnixConn, request ownerRequest) (acceptedOwnerWork, error) {
	intent, assumption, publication, operation, err := s.readResident(ctx)
	if err != nil {
		return acceptedOwnerWork{}, err
	}
	if err = authenticateOwnerRequest(ctx, connection, intent, assumption, publication, request); err != nil {
		return acceptedOwnerWork{}, err
	}
	if err = validateRuntimeRestartRequest(operation, request); err != nil {
		return acceptedOwnerWork{}, err
	}
	work := acceptedOwnerWork{request: request, operation: operation}
	if ownPreparationAction(request.Action) || request.Action == ownerRuntimeRestartPreflight {
		work.selected, work.selectedReference, err = resolveSelectedContext(ctx, s.journal, operation.defaultContextReference(), request.Context)
		if err != nil {
			return acceptedOwnerWork{}, err
		}
		if request.Action == ownerRuntimeRestartPreflight && work.selectedReference != operation.defaultContextReference() {
			return acceptedOwnerWork{}, ErrIdentity
		}
	}
	// Native/record authentication and context reads precede the atomic closing
	// decision. No host work executes before this registration succeeds.
	if err = s.checkInitialRequest(work); err != nil {
		return acceptedOwnerWork{}, err
	}
	if err = s.admitConnection(connection, request.Action); err != nil {
		return acceptedOwnerWork{}, err
	}
	return work, nil
}

func (s *ownerServer) readResident(ctx context.Context) (StartupIntent, StartupAssumption, OwnerPublication, Operation, error) {
	e := s.entry
	e.mu.Lock()
	defer e.mu.Unlock()
	var assumption StartupAssumption
	var publication OwnerPublication
	if e.closed || e.journal == nil || e.lease == nil {
		return StartupIntent{}, assumption, publication, Operation{}, os.ErrClosed
	}
	intent := e.operation.Intent()
	err := e.journal.withOpen(ctx, func() error {
		return e.lease.WithHeld(ctx, func() error {
			if err := e.validateResidentHeld(ctx); err != nil {
				return err
			}
			reference, err := ReferenceStartup(intent)
			if err != nil {
				return err
			}
			reader := controlOwnerReader{controlIntentReader{e.journal.control}}
			assumption, err = readAssumptionRecord(ctx, reader, e.journal.pin, reference)
			if err != nil {
				return err
			}
			publication, err = readPublicationRecord(ctx, reader, e.journal.pin, reference)
			return err
		})
	})
	return intent, assumption, publication, e.operation, err
}

func ownerServerReply(request ownerRequest, code ownerReplyCode) ownerReply {
	return ownerReply{Protocol: request.Protocol, RootID: request.RootID, OwnerRef: request.Owner, PublicationSHA256: request.PublicationSHA256, Action: request.Action, Target: request.Target, Context: request.Context, Source: request.Source, RestartFrom: request.RestartFrom, Initial: request.Initial, InitialResultFor: request.InitialResultFor, Code: code}
}

func (s *ownerServer) executeWork(ctx context.Context, work acceptedOwnerWork) (ownerReply, bool) {
	reply := ownerServerReply(work.request, ownerOK)
	switch work.request.Action {
	case ownerStatus:
		// Status confirms authenticated resident contact, never daemon readiness.
	case ownerRuntimeRestartPreflight:
		return s.executeRuntimeRestartPreflight(ctx, work, reply), false
	case ownerEnsureOwn, ownerRecoverSaved, ownerPrepareContinuation, ownerMigrateOwn, ownerRuntimeRestart:
		return s.executeOwnPreparation(ctx, work, reply)
	case ownerEnsureOther:
		return s.executeOtherPreparation(ctx, work, reply), false
	default:
		reply.Code = ownerIncompatible
	}
	return reply, false
}

func (s *ownerServer) executeOwnPreparation(ctx context.Context, work acceptedOwnerWork, reply ownerReply) (ownerReply, bool) {
	outcome, replayed, err := s.preparationForRequest(ctx, work)
	if outcome == (PreparationOutcome{}) {
		s.failReply(&reply, err)
		return reply, false
	}
	reply.Code, reply.Disposition = ownerReplyCode(outcome.Condition), outcome.Disposition
	reply.Report, reply.Failure = outcome.Report, outcome.Failure
	reply.RuntimeRestart = outcome.RuntimeRestart
	reply.Target = originRecord{Root: work.operation.Intent().Inputs.Root, RootID: work.operation.Intent().Inputs.RootID}
	reply.Startup = work.request.Owner
	return reply, replayed
}

func (s *ownerServer) failReply(reply *ownerReply, err error) {
	reply.Code = ownerFault
	if retainsResidentCleanup(err) {
		s.connectionFault(err)
		return
	}
	if reply.Report == (ManualMigrationReport{}) && s.host.ClassifyPreparation(err).runtimeRestartPending() {
		reply.Code = ownerBusy
		if reply.Startup != (StartupReference{}) {
			reply.Code = ownerPending
		}
		return
	}
	if s.host.ClassifyPreparation(err).runtimeQuiescenceBusy() {
		reply.Code = ownerBusy
		if reply.RuntimeRestart != (RuntimeRestartReport{}) {
			reply.Code = ownerPending
		}
		return
	}
	if reply.Report == (ManualMigrationReport{}) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, task.ErrLockBusy) || errors.Is(cause, ErrOwnerBusy)
	}) {
		reply.Code = ownerBusy
		if reply.RuntimeRestart != (RuntimeRestartReport{}) {
			reply.Code = ownerPending
		}
		return
	}
	s.connectionFault(err)
	switch {
	case reply.Report == (ManualMigrationReport{}) && reply.RuntimeRestart == (RuntimeRestartReport{}) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, ErrOwnerIncompatible)
	}):
		reply.Code = ownerIncompatible
	case errors.Is(err, context.DeadlineExceeded):
		reply.Code = ownerTimeout
	}
}

// Serialize deadline extension with closing so an incomplete connection cannot
// overwrite the stop-admission wakeup and delay draining.
func (s *ownerServer) setRequestDeadline(connection *net.UnixConn, deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return os.ErrClosed
	}
	return connection.SetDeadline(deadline)
}

func validateOwnerServerDisposition(disposition Disposition, selected LaunchContextReference) error {
	if err := disposition.Validate(); err != nil {
		return err
	}
	if disposition.Kind == DispositionStarted && disposition.Context != selected {
		return ErrIdentity
	}
	return nil
}

// Busy is an authenticated, exact-subject non-admission result. Its bounded
// transport write never extends the original request's finite deadline.
func (s *ownerServer) setBusyDeadline(connection *net.UnixConn, ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return os.ErrClosed
	}
	deadline := time.Now().Add(ownerBusyWriteTimeout)
	if original, finite := ctx.Deadline(); finite && original.Before(deadline) {
		deadline = original
	}
	return connection.SetWriteDeadline(deadline)
}
