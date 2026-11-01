package host

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestHostDeathBeforeClosureReceiptKeepsAcceptedStopPending(t *testing.T) {
	config := testConfig(t, "hold")
	execution := holdPriorExecution(t, config)
	process := startHostFixture(t, config)
	cleanupExecutionBeforeHost(t, execution)
	query := queryFor(t, config)
	eventuallyStatus(t, query, func(status Status) bool {
		return status.LastError == "waiting_for_prior_runtime" && status.Child == nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	pending, err := Stop(ctx, query)
	assertAcceptedStopPending(t, pending, err)
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("stopping generation: %t %v", present, err)
	}
	pinLostHostEndpoint(t, process, epoch.Endpoint)
	if _, err = process.control.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	receive(t, process.done)
	var exit *exec.ExitError
	if !errors.As(process.err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("fixture did not take abrupt exit: %v", process.err)
	}
	ctx, cancelAgain := context.WithTimeout(t.Context(), time.Second)
	defer cancelAgain()
	recovered, err := Stop(ctx, query)
	assertAcceptedStopPending(t, recovered, err)
}
