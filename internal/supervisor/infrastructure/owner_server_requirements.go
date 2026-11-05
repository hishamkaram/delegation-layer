package infrastructure

import (
	"context"
	"errors"
	"net"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Only the post-authentication compatibility gate returns this private marker.
// Auth, record and cleanup errors must never be reclassified by a public cause.
var errOwnerRequirementsIncompatible = errors.New("infrastructure owner artifacts incompatible")

func checkOwnRequirements(request ownerRequest, operation Operation) error {
	if !ownPreparationAction(request.Action) || request.Requirements == (ArtifactRequirements{}) {
		return nil
	}
	err := request.Requirements.ValidateArtifacts(operation.Artifacts())
	if err != nil && !retainsResidentCleanup(err) && task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrIncompatible) }) {
		return errOwnerRequirementsIncompatible
	}
	return err
}

func (s *ownerServer) refuseOwnerWork(ctx context.Context, connection *net.UnixConn, request ownerRequest, err error) {
	reply := ownerServerReply(request, ownerRefused)
	switch {
	case retainsResidentCleanup(err):
		s.connectionFault(err)
	case task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, errOwnerRequirementsIncompatible) }):
		reply.Code = ownerIncompatible
	case task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, errOwnerWorkBusy) }):
		reply.Code = ownerBusy
	default:
		s.connectionFault(err)
		if errors.Is(err, context.DeadlineExceeded) {
			reply.Code = ownerTimeout
		}
	}
	if reply.Code == ownerBusy || reply.Code == ownerIncompatible {
		if deadlineErr := s.setBusyDeadline(connection, ctx); deadlineErr != nil {
			s.connectionFault(deadlineErr)
			return
		}
	}
	if writeErr := writeOwnerFrame(connection, reply); writeErr != nil {
		s.connectionFault(writeErr)
	}
}
