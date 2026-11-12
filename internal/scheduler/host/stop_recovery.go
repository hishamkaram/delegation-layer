package host

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Only explicit Stop can reestablish this exact record's durability. Passive
// Observe remains a reader and never performs the verification barrier.
type stopReader interface {
	ReadSchedulerHostRecordContext(context.Context, string) ([]byte, error)
	VerifySchedulerHostRecordContext(context.Context, string, []byte) error
}

func recoverStoppedFence(ctx context.Context, query Query, epoch epochRecord, response wireResponse, requestErr error) (StopResult, error) {
	observed := StopResult{Status: Status{State: "degraded", Epoch: epoch.Epoch, Protocol: epoch.Protocol, BuildID: epoch.BuildID, Host: epoch.Host, LastError: "host_unreachable"}}
	if response.Status.Epoch != "" {
		status, err := response.Status.value()
		if err != nil {
			requestErr = errors.Join(requestErr, ErrIdentity, err)
		} else {
			observed.Status = status
			observed.Accepted = response.Accepted
		}
	}
	recovered, fenceErr := awaitStoppedFence(ctx, query.Store, epoch)
	if errors.Is(fenceErr, fs.ErrNotExist) && task.AllErrorCauses(fenceErr, func(cause error) bool { return errors.Is(cause, fs.ErrNotExist) }) {
		if noHostConnection(requestErr) {
			observed.Status.State, observed.Status.LastError, observed.Status.HostReady = "no_host_observed", "", false
		}
		return observed, stopReplyError(requestErr)
	}
	if fenceErr != nil {
		if !recovered.Accepted {
			recovered = observed
		}
		return recovered, errors.Join(stopReplyError(requestErr), fenceErr)
	}
	// An exact durable fence replaces only transport uncertainty. Authentication,
	// protocol and storage faults remain visible alongside recovered acceptance.
	if requestErr == nil || lostReplyOnly(requestErr) || noHostConnection(requestErr) {
		requestErr = nil
	}
	return finishStop(ctx, recovered, requestErr)
}

func awaitStoppedFence(ctx context.Context, reader stopReader, epoch epochRecord) (last StopResult, resultErr error) {
	for {
		observed, err := readStoppedFence(ctx, reader, epoch)
		if observed.Accepted || !last.Accepted {
			last = observed
		}
		if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrPending) }) {
			return last, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, errors.Join(ErrPending, ctx.Err())
		case <-timer.C:
		}
	}
}

func readStoppedFence(ctx context.Context, reader stopReader, epoch epochRecord) (StopResult, error) {
	result := StopResult{Status: Status{State: "stopping", Epoch: epoch.Epoch, Protocol: epoch.Protocol, BuildID: epoch.BuildID, Host: epoch.Host}}
	data, err := reader.ReadSchedulerHostRecordContext(ctx, stopName(epoch.Sequence))
	if err != nil {
		return result, err
	}
	var record stopRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return result, err
	}
	if record.RootID != epoch.RootID || record.Epoch != epoch.Epoch || record.Host != epoch.Host {
		return result, ErrIdentity
	}
	expected, err := task.MarshalCanonical(record)
	if err != nil {
		return result, err
	}
	if err = reader.VerifySchedulerHostRecordContext(ctx, stopName(epoch.Sequence), expected); err != nil {
		return result, err
	}
	result.Accepted, result.Status.StopAccepted, result.Status.Attempt = true, true, record.Attempt
	if err = verifyStoppedClosure(ctx, reader, epoch, record); err != nil {
		return result, err
	}
	if record.Child != (Identity{}) && !record.ChildUncertain {
		if _, err = lifetime.NewProcessRef(record.Child.PID, record.Child.StartToken); err != nil {
			return result, errors.Join(ErrIdentity, err)
		}
		result.Status.Child = &record.Child
	}
	return result, nil
}

func verifyStoppedClosure(ctx context.Context, reader stopReader, epoch epochRecord, record stopRecord) error {
	closure, err := reader.ReadSchedulerHostRecordContext(ctx, closedName(epoch.Sequence))
	if errors.Is(err, fs.ErrNotExist) && task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, fs.ErrNotExist) }) {
		return ErrPending
	}
	if err != nil {
		return err
	}
	var closed closedRecord
	if err = task.DecodeStrict(closure, &closed); err != nil {
		return err
	}
	if closed.Stop != record {
		return ErrIdentity
	}
	canonical, err := task.MarshalCanonical(closed)
	if err != nil {
		return err
	}
	return reader.VerifySchedulerHostRecordContext(ctx, closedName(epoch.Sequence), canonical)
}

func lostReplyOnly(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, io.EOF) || errors.Is(cause, net.ErrClosed) || errors.Is(cause, os.ErrClosed) || errors.Is(cause, os.ErrDeadlineExceeded) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, syscall.ECONNRESET) || errors.Is(cause, syscall.EPIPE)
	})
}

func stopReplyError(err error) error {
	if lostReplyOnly(err) || noHostConnection(err) {
		return ErrPending
	}
	return errors.Join(ErrPending, err)
}

func noHostConnection(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, fs.ErrNotExist) || errors.Is(cause, syscall.ECONNREFUSED)
	})
}

func finishStop(ctx context.Context, result StopResult, requestErr error) (StopResult, error) {
	if err := awaitIdentityAbsent(ctx, result.Status.Host); err != nil {
		return result, errors.Join(ErrPending, requestErr, err)
	}
	if result.Status.Child != nil {
		if err := awaitIdentityAbsent(ctx, *result.Status.Child); err != nil {
			return result, errors.Join(ErrPending, requestErr, err)
		}
	}
	result.Stopped, result.Status.Ready, result.Status.HostReady, result.Status.State = true, false, false, "stopped"
	return result, requestErr
}
