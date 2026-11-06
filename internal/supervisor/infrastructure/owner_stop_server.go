package infrastructure

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (s *ownerServer) handleStopConnection(parent context.Context, connection *net.UnixConn, request ownerStopRequest) {
	authentication := request.authenticationRequest()
	ctx, cancel, err := ownerContactContext(parent, authentication)
	if err != nil {
		s.connectionFault(err)
		return
	}
	defer cancel()
	deadline, _ := ctx.Deadline() // The existing contact boundary requires one.
	if err = s.setRequestDeadline(connection, deadline); err != nil {
		s.connectionFault(err)
		return
	}
	intent, assumption, publication, _, err := s.readResident(ctx)
	if err == nil {
		err = authenticateOwnerRequest(ctx, connection, intent, assumption, publication, authentication)
	}
	if err != nil {
		s.connectionFault(err)
		return
	}
	// Reserve a control slot before stopping admission. Pre-auth connections are
	// woken on stop; this authenticated reply must retain its own wire deadline.
	code := ownerStopBusy
	if err = s.admitConnection(connection, ownerStatus); err != nil {
		if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, errOwnerWorkBusy) }) {
			s.connectionFault(err)
			return
		}
		if err = s.setBusyDeadline(connection, ctx); err != nil {
			s.connectionFault(err)
			return
		}
	} else {
		code = s.commitOwnerStop()
	}
	reply := ownerStopReply{StopProtocol: ownerStopProtocolVersion, RootID: request.RootID, Owner: request.Owner, PublicationSHA256: request.PublicationSHA256, Code: code}
	if err = writeOwnerFrame(connection, reply); err != nil {
		s.connectionFault(err)
	}
}

// The server owns admission. Its mutex serializes the empty-work decision and
// permanent stop; busy changes no serving state. The runtime retains ownership
// of daemon Wait registration and completion independently of the stop reply.
func (s *ownerServer) commitOwnerStop() ownerStopCode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleanupFailure != nil {
		return ownerStopFault
	}
	if s.closing || !s.initialCompleted || s.connectionCount(ownerConnectionOperation) != 0 {
		return ownerStopBusy
	}
	if s.terminalHandoff && !s.deliveryCompleted {
		deadline, err := canonicalStartupTime(s.entry.operation.Intent().Deadline)
		if err != nil {
			return ownerStopFault
		}
		if time.Now().Before(deadline) {
			return ownerStopBusy
		}
	}
	s.stopAcceptingHeld()
	return ownerStopAccepted
}
