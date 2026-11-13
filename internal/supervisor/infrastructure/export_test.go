package infrastructure

import (
	"context"
	"testing"
	"time"
)

// RuntimeRestartWireForTest connects an external application classifier to the
// existing real-store/Unix server fixture. It exists only in the test image;
// production authority and launch APIs are unchanged.
func RuntimeRestartWireForTest(t *testing.T, cause error, classify func(error) PreparationFailure) (PreparationOutcome, error) {
	t.Helper()
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{
		classify: classify,
		restart: func(_ context.Context, operation Operation, _ LaunchContext, from StartupReference, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, RuntimeRestartReport, error) {
			current, err := ReferenceStartup(operation.Intent())
			if err != nil {
				return Disposition{}, RuntimeRestartReport{}, err
			}
			return Disposition{}, RuntimeRestartReport{From: from, Phase: RuntimeRestartSuccessorAccepted, Successor: current}, cause
		},
	}, ownerServerFixtureSelection{initial: InitialRuntimeRestart})
	// The injected retained marker has no resource of its own. Preserve its
	// expected server fault while still closing the fixture's actual borrowers.
	f.expectedCleanupFault = cause
	f.start()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner, err := ReferenceStartup(f.entry.operation.Intent())
	if err != nil {
		t.Fatal(err)
	}
	return RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), owner, PreparationRequest{
		Operation: f.entry.operation.Intent().Inputs.Initial,
		Context:   f.entry.operation.defaultContextReference(),
	})
}
