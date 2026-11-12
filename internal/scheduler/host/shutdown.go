package host

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The H owner remains control-responsive through every joined shutdown wait.
// No cancellation or missing heartbeat is treated as prior-runtime closure.
func (s *service) joinStoppedChild() error {
	if s.child == nil {
		return nil
	}
	closeErr := s.child.control.stopSending()
	for {
		select {
		case waitErr := <-s.child.wait:
			if task.AllErrorCauses(waitErr, busyExit) {
				waitErr = nil
			}
			closeErr = errors.Join(closeErr, waitErr, s.child.control.close())
			s.child = nil
			return closeErr
		case request := <-s.requests:
			s.replyStopping(request)
		}
	}
}

func (s *service) awaitStoppedExecution(ctx context.Context) (*scheduler.OwnerLease, error) {
	for {
		lease, err := scheduler.AcquireOwnerLease(ctx, s.config.Store)
		if err == nil || !lockBusyOnly(err) {
			return lease, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-timer.C:
		case request := <-s.requests:
			timer.Stop()
			s.replyStopping(request)
		}
	}
}

func (s *service) replyStopping(request controlRequest) {
	response := controlResponse{wireResponse: wireResponse{Status: statusWire(s.snapshot())}}
	if request.request.Action == "health" {
		response.health = &InstallationHealth{State: "unknown", Reason: "installation_host_stopping"}
	}
	if request.request.Action == "stop" {
		response.Accepted = s.status.StopAccepted
		if !response.Accepted {
			response.Code = "storage_fault"
		}
	}
	request.reply <- response
}
