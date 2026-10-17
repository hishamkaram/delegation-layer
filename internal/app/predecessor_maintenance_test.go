package app

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestPredecessorTerminatedPreservesSubmissionDeferral(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateStashed)
	prepareCompleteStashedObservationClient(t, fixture)
	prepareBusyAdmissionMigration(t, fixture.root, fixture.predecessor, fixture.options)
	before := snapshotObservationPhaseRecords(t, fixture)
	commands := observePhaseStatusCommands(fixture)

	err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, tasklifecycle.SupervisorOptions{
		Options: fixture.options, InitialExecutable: fixture.clientA, OpenStore: openNativeSupervisorStore,
	})
	if !errors.Is(err, pueue.ErrUnknown) || !errors.Is(err, pueue.ErrPrivateSupervisorSubmissionIncomplete) || !errors.Is(err, pueue.ErrBinding) {
		t.Errorf("predecessor lost submission deferral identities: %v", err)
	}
	if errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("submission deferral invented migration-required identity: %v", err)
	}
	assertObservationPhaseNoMutation(t, commands())
	assertObservationPhaseRecords(t, before)
	assertHistoricalTaskEvidence(t, fixture)
}

func TestPredecessorTerminatedNonEndedRemainsUnknown(t *testing.T) {
	for _, state := range []pueue.State{pueue.StateRunning, pueue.StateQueued} {
		t.Run(string(state), func(t *testing.T) {
			fixture := prepareHistoricalObservationPhase(t, state)
			prepareCompleteStashedObservationClient(t, fixture)
			before := snapshotObservationPhaseRecords(t, fixture)
			commands := observePhaseStatusCommands(fixture)

			err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, tasklifecycle.SupervisorOptions{
				Options: fixture.options, InitialExecutable: fixture.clientA, OpenStore: openNativeSupervisorStore,
			})
			if !errors.Is(err, pueue.ErrUnknown) || errors.Is(err, pueue.ErrPrivateSupervisorSubmissionIncomplete) || errors.Is(err, pueue.ErrBinding) || errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
				t.Errorf("ordinary non-ended predecessor changed error identity: %v", err)
			}
			assertObservationPhaseNoMutation(t, commands())
			assertObservationPhaseRecords(t, before)
			assertHistoricalTaskEvidence(t, fixture)
		})
	}
}
