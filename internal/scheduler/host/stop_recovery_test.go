package host

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestHostStopRecoversAcknowledgementFromExactDurableFence(t *testing.T) {
	config := testConfig(t, "hold")
	process := startHostFixture(t, config)
	query := queryFor(t, config)
	ready := eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	first, err := Stop(ctx, query)
	if err != nil || !first.Accepted || !first.Stopped {
		t.Fatalf("initial fenced stop: %+v %v", first, err)
	}
	receive(t, process.done)
	if process.err != nil {
		t.Fatal(process.err)
	}
	// The endpoint is now gone. A caller whose first acknowledgement was lost
	// must recover the same fence, not redirect to another generation or say
	// that the original stop was not accepted.
	recovered, err := Stop(ctx, query)
	if err != nil || !recovered.Accepted || !recovered.Stopped || recovered.Status.Epoch != ready.Epoch {
		t.Fatalf("lost acknowledgement recovery: %+v %v", recovered, err)
	}
	epoch, present, err := readLatest(ctx, query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("fenced generation: %t %v", present, err)
	}
	fault := errors.New("independent stop protocol fault")
	recovered, err = recoverStoppedFence(ctx, query, epoch, wireResponse{}, errors.Join(io.EOF, fault))
	if !recovered.Accepted || !recovered.Stopped || !errors.Is(err, fault) {
		t.Fatalf("recovered fence erased a concrete fault: %+v %v", recovered, err)
	}
	assertLeasesFree(t, config)
}

func TestHostStopWaitsForPriorExecutionAndAnswersControls(t *testing.T) {
	config := testConfig(t, "hold")
	execution := holdPriorExecution(t, config)
	process := startHostFixture(t, config)
	cleanupExecutionBeforeHost(t, execution)
	query := queryFor(t, config)
	eventuallyStatus(t, query, func(status Status) bool {
		return status.LastError == "waiting_for_prior_runtime" && status.Child == nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	pending, err := Stop(ctx, query)
	assertAcceptedStopPending(t, pending, err)
	assertStoppingControls(t, query)
	select {
	case <-process.done:
		t.Fatal("host released H while the predecessor still held E")
	default:
	}
	if err = execution.Close(); err != nil {
		t.Fatal(err)
	}
	finished, err := Stop(t.Context(), query)
	if err != nil || !finished.Accepted || !finished.Stopped {
		t.Fatalf("released predecessor did not complete stop: %+v %v", finished, err)
	}
	receive(t, process.done)
	if process.err != nil {
		t.Fatal(process.err)
	}
	assertLeasesFree(t, config)
}

func holdPriorExecution(t *testing.T, config Config) *scheduler.OwnerLease {
	t.Helper()
	execution, err := scheduler.AcquireOwnerLease(t.Context(), config.Store)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionBeforeHost(t, execution)
	return execution
}

func cleanupExecutionBeforeHost(t *testing.T, execution *scheduler.OwnerLease) {
	t.Helper()
	t.Cleanup(func() {
		if err := execution.Close(); err != nil {
			t.Error(err)
		}
	})
}

func assertAcceptedStopPending(t *testing.T, result StopResult, err error) {
	t.Helper()
	pendingOnly := task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, ErrPending) || errors.Is(cause, context.DeadlineExceeded)
	})
	if !result.Accepted || result.Stopped || !pendingOnly {
		t.Fatalf("accepted stop lost uncertainty or invented a concrete fault: %+v %v", result, err)
	}
}

func assertStoppingControls(t *testing.T, query Query) {
	t.Helper()
	status, err := Observe(t.Context(), query)
	if err != nil || status.State != "stopping" || !status.StopAccepted || status.Ready {
		t.Fatalf("joined stop did not answer controls: %+v %v", status, err)
	}
}

func TestHostAcknowledgedStopSurvivesReadbackFailure(t *testing.T) {
	config := testConfig(t, "hold")
	query := queryFor(t, config)
	if err := query.Store.Close(); err != nil {
		t.Fatal(err)
	}
	epoch := epochRecord{RootID: config.Store.RootID, Epoch: "0123456789abcdef0123456789abcdef", Host: Identity{PID: 1, StartToken: "exact"}}
	response := wireResponse{Accepted: true, Status: statusWire(Status{State: "stopping", Epoch: epoch.Epoch, StopAccepted: true})}
	result, err := recoverStoppedFence(t.Context(), query, epoch, response, nil)
	if !result.Accepted || result.Stopped || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("validated acknowledgement was erased by read failure: %+v %v", result, err)
	}
}

func TestRefusedStopRetainsLifecycleReadinessWithoutClaimingInstallationHealth(t *testing.T) {
	config := testConfig(t, "hold")
	query := queryFor(t, config)
	if err := query.Store.Close(); err != nil {
		t.Fatal(err)
	}
	epoch := epochRecord{RootID: config.Store.RootID, Epoch: "0123456789abcdef0123456789abcdef", Host: Identity{PID: 1, StartToken: "exact"}}
	response := wireResponse{Status: wireStatus{State: "ready", Ready: true, Epoch: epoch.Epoch}}
	result, err := recoverStoppedFence(t.Context(), query, epoch, response, errors.New("stop refused"))
	if err == nil || result.Accepted || result.Status.Ready || !result.Status.HostReady || result.Status.Health != nil {
		t.Fatalf("refused stop confused lifecycle and installation readiness: %+v %v", result, err)
	}
}
