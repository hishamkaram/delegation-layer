package infrastructure

import (
	"context"
	"errors"
	"net"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (s *ownerServer) refuseOwnerWork(ctx context.Context, connection *net.UnixConn, request ownerRequest, err error) {
	reply := ownerServerReply(request, ownerRefused)
	switch {
	case retainsResidentCleanup(err):
		s.connectionFault(err)
	case task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, errOwnerWorkBusy) }):
		reply.Code = ownerBusy
	default:
		s.connectionFault(err)
		if errors.Is(err, context.DeadlineExceeded) {
			reply.Code = ownerTimeout
		}
	}
	if reply.Code == ownerBusy {
		if deadlineErr := s.setBusyDeadline(connection, ctx); deadlineErr != nil {
			s.connectionFault(deadlineErr)
			return
		}
	}
	if writeErr := writeOwnerFrame(connection, reply); writeErr != nil {
		s.connectionFault(writeErr)
	}
}
