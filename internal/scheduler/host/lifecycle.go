package host

import (
	"context"
	"errors"
	"time"
)

func (s *service) binding(kind string) childFrame {
	return childFrame{Kind: kind, Protocol: ProtocolVersion, RootID: s.epoch.RootID, Epoch: s.epoch.Epoch, Attempt: s.status.Attempt, BuildID: s.epoch.BuildID, Host: s.epoch.Host, Child: s.child.identity}
}

func (s *service) childEvent(ctx context.Context, event childEvent) {
	if event.err != nil {
		if !s.permanent {
			s.status.State, s.status.LastError = "degraded", "child_channel_closed"
		}
		return
	}
	if s.permanentChildFault(event) {
		return
	}
	expected := s.binding(event.frame.Kind)
	if event.frame != expected {
		s.degrade("child_identity_fault")
		return
	}
	switch event.frame.Kind {
	case "ready":
		if s.permanent || s.status.StopAccepted {
			return
		}
		if err := validateStore(ctx, s.config.Store); err != nil {
			s.degrade("root_identity_fault")
			return
		}
		confirmed := s.binding("confirmed")
		if err := writeFrame(s.child.control.writer, confirmed); err != nil {
			s.status.State, s.status.LastError = "degraded", "child_confirmation_failed"
			return
		}
		if s.readiness != nil {
			s.readiness.Stop()
			s.readiness = nil
		}
		s.readyAt = s.clock.Now()
		s.status.State, s.status.LastError = "ready", ""
	default:
		s.degrade("child_protocol_fault")
	}
}

// The host alone applies terminal policy. Both the live reader and the final
// reader drain authenticate the same immutable binding before degrading.
func (s *service) permanentChildFault(event childEvent) bool {
	if event.err != nil {
		return false
	}
	var code string
	switch event.frame.Kind {
	case "incompatible":
		code = "incompatible_definition"
	case "cleanup_unresolved":
		code = "cleanup_unresolved"
	default:
		return false
	}
	if s.status.StopAccepted {
		return true
	}
	if event.frame != s.binding(event.frame.Kind) {
		code = "child_identity_fault"
	}
	s.degrade(code)
	return true
}

func (s *service) degrade(code string) {
	s.permanent = true
	s.status.State, s.status.LastError = "degraded", code
	if s.retry != nil {
		s.retry.Stop()
		s.retry = nil
	}
}

func (s *service) exited(waitErr error) {
	// Wait may beat the reader's last terminal frame. Join the reader
	// and drain all its bounded messages before deciding whether to restart.
	s.joinChildReader()
	closeErr := s.child.control.close()
	s.child = nil
	if s.readiness != nil {
		s.readiness.Stop()
		s.readiness = nil
	}
	if closeErr != nil {
		s.degrade("child_channel_close_failed")
	}
	if s.permanent || s.status.StopAccepted {
		return
	}
	healthy := time.Duration(0)
	if !s.readyAt.IsZero() {
		healthy = s.clock.Now().Sub(s.readyAt)
	}
	if busyExit(waitErr) && s.readyAt.IsZero() {
		s.scheduleRetry("waiting_for_prior_runtime", 0)
		return
	}
	s.scheduleRetry("child_exited", healthy)
}

func (s *service) scheduleRetry(code string, healthy time.Duration) {
	var delay time.Duration
	s.failures, delay = afterExit(s.failures, healthy)
	s.status.State, s.status.LastError = "backoff", code
	if code == "waiting_for_prior_runtime" {
		s.status.State = "starting"
	}
	next := s.clock.Now().Add(delay).UTC()
	s.status.NextRetryAt = &next
	s.retry = s.clock.NewTimer(delay)
}

func (s *service) fenceStop(ctx context.Context) (bool, error) {
	if s.stopIntent == nil {
		record := stopRecord{RootID: s.epoch.RootID, Epoch: s.epoch.Epoch, Host: s.epoch.Host, Attempt: s.status.Attempt}
		if s.child != nil {
			record.Child = s.child.identity
			record.ChildUncertain = record.Child.PID <= 0 || record.Child.StartToken == ""
		}
		s.stopIntent = &record
	}
	committed, err := publishRecord(ctx, s.journal, stopName(s.epoch.Sequence), *s.stopIntent)
	if committed {
		s.status.StopAccepted = true
		s.status.State, s.status.NextRetryAt = "stopping", nil
	} else if err != nil {
		// An unresolved create-once fence never permits another child launch.
		// Retries retain the original candidate, including its child snapshot.
		s.degrade("stop_fence_unverified")
	}
	return committed, err
}

func (s *service) control(ctx context.Context, request controlRequest) (bool, error) {
	switch request.request.Action {
	case "health":
		health := s.installationHealth(ctx)
		request.reply <- controlResponse{wireResponse: wireResponse{Status: statusWire(s.snapshot())}, health: &health}
		return false, nil
	case "status":
		request.reply <- controlResponse{wireResponse: wireResponse{Status: statusWire(s.snapshot())}}
		return false, nil
	case "stop":
		committed, err := s.fenceStop(ctx)
		response := wireResponse{Status: statusWire(s.snapshot()), Accepted: committed}
		if err != nil {
			response.Code = "storage_fault"
		} else if s.stopIntent.ChildUncertain {
			response.Code = "identity"
		}
		request.reply <- controlResponse{wireResponse: response}
		if committed {
			// H stays held while the bounded handler completes its accepted
			// response. Server cancellation cannot win its reply select.
			if servedErr := <-request.served; !lostReplyOnly(servedErr) {
				err = errors.Join(err, servedErr)
			}
		}
		return committed, err
	default:
		request.reply <- controlResponse{wireResponse: wireResponse{Status: statusWire(s.snapshot()), Code: "invalid_control"}}
		return false, nil
	}
}

func (s *service) snapshot() Status {
	status := s.status
	status.Fault = s.permanent
	status.Child = nil
	if s.child != nil && s.child.identity.PID > 0 && s.child.identity.StartToken != "" {
		child := s.child.identity
		status.Child = &child
	}
	status.Ready = false
	status.HostReady = status.State == "ready" && s.child != nil && !s.permanent && !status.StopAccepted
	return status
}

func (s *service) close(ctx context.Context) (closeErr error) {
	if s.retry != nil {
		s.retry.Stop()
	}
	if s.readiness != nil {
		s.readiness.Stop()
	}
	closeErr = s.joinStoppedChild()
	s.joinInstallationProbe()
	lease, leaseErr := s.awaitStoppedExecution(ctx)
	closeErr = errors.Join(closeErr, leaseErr)
	if lease != nil {
		defer func() { closeErr = errors.Join(closeErr, lease.Close()) }()
		if s.status.StopAccepted && closeErr == nil {
			_, receiptErr := publishRecord(ctx, s.journal, closedName(s.epoch.Sequence), closedRecord{Stop: *s.stopIntent})
			closeErr = errors.Join(closeErr, receiptErr)
		}
	}
	s.cancelServer()
	closeErr = errors.Join(closeErr, s.endpoint.close(), <-s.serverDone)
	return closeErr
}

func (s *service) joinChildReader() {
	joined := false
	for !joined {
		select {
		case <-s.child.control.done:
			joined = true
		case event := <-s.child.control.events:
			s.permanentChildFault(event)
		}
	}
	s.drainChildFrames()
}

func (s *service) drainChildFrames() {
	for len(s.child.control.events) > 0 {
		event := <-s.child.control.events
		s.permanentChildFault(event)
	}
}
