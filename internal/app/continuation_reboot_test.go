package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// These are desired-behavior regressions for a machine reboot that left a
// provider turn without provider.exit or outcome.json. The queue row is Ended
// and the native lifetime journal is closed by previous-boot evidence, so the
// provider session can safely be handed to a successor.

func TestPredecessorTerminatedReleasesRebootClosedContinuation(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot})
	requireRebootClosedPreconditions(t, fixture)
	requireRebootSuccessorBusy(t, fixture)
	before := snapshotRebootTaskFiles(t, fixture.td.Dir)

	if err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
		t.Fatalf("reboot-closed predecessor was not released for continuation: %v", err)
	}
	if err := fixture.successor.ClaimSession(fixture.req.Provider, fixture.req.PriorSession.ConversationID); err != nil {
		t.Fatalf("successor could not claim the reboot-released session: %v", err)
	}
	requireRebootNoProviderPublication(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)
}

func TestPredecessorTerminatedAcceptsRebootClosedFirstTurn(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{identity: true, stage: rebootLifetimeClosedPreviousBoot})
	requireRebootClosedPreconditions(t, fixture)
	before := snapshotRebootTaskFiles(t, fixture.td.Dir)

	if err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
		t.Fatalf("reboot-closed first-turn predecessor was refused: %v", err)
	}
	requireRebootNoProviderPublication(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)
}

func TestResolvePredecessorAcceptsRebootClosedContinuation(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot})
	requireRebootClosedPreconditions(t, fixture)
	request, options := rebootContinuationAdmission(t, fixture)

	resolved, err := tasklifecycle.ResolvePredecessor(fixture.store, options, request, fixture.req.TaskID)
	if err != nil {
		t.Fatalf("resolve-predecessor refused a reboot-closed predecessor: %v", err)
	}
	if resolved.Provider != fixture.req.Provider || resolved.ConversationID != fixture.req.PriorSession.ConversationID || resolved.PredecessorTaskID != fixture.req.TaskID {
		t.Fatalf("resolved continuation = %+v", resolved)
	}
	if err = fixture.successor.ClaimSession(fixture.req.Provider, resolved.ConversationID); err != nil {
		t.Fatalf("successor could not claim the reboot-released session: %v", err)
	}
}

// The controls below share the Ended queue row and must stay refused: the queue
// alone, an open lifetime journal, or a live runner lease is not a handoff.
func TestPredecessorTerminatedRefusesRebootWithoutClosedLifetime(t *testing.T) {
	cases := []struct {
		name  string
		setup rebootSetup
	}{
		{name: "queue row Ended without lifetime records", setup: rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeNone}},
		{name: "open registered lifetime with unprovable boot identity", setup: rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeOpen}},
		{name: "closed lifetime with held runner lease", setup: rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, holdRunnerLease: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, tc.setup)
			requireRebootNoProviderPublication(t, fixture)
			requireRebootSuccessorBusy(t, fixture)
			before := snapshotRebootTaskFiles(t, fixture.td.Dir)

			if err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err == nil {
				t.Fatal("predecessor without closed lifetime and released runner lease was accepted")
			}
			requireRebootSuccessorBusy(t, fixture)
			requireRebootNoProviderPublication(t, fixture)
			requireRebootTaskFilesUnchanged(t, before)
		})
	}
}

// The third reported reboot had no provider.ref.json but an immutable
// requested PriorSession. That recorded input is the session reference, bound
// to the immediate predecessor rather than the inherited ancestor.
func TestContinuationPriorUsesRecordedInputWhenRebootPredecessorHasNoIdentity(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), stage: rebootLifetimeClosedPreviousBoot})
	requireRebootClosedLifetime(t, fixture.td)
	requireRebootNoProviderPublication(t, fixture)
	before := snapshotRebootTaskFiles(t, fixture.td.Dir)

	prior, err := tasklifecycle.ContinuationPrior(fixture.td, &fixture.req)
	want := task.PriorSession{Provider: fixture.req.Provider, ConversationID: fixture.req.PriorSession.ConversationID, PredecessorTaskID: fixture.req.TaskID}
	if err != nil || prior == nil || *prior != want || prior.PredecessorTaskID == testPriorSession().PredecessorTaskID {
		t.Fatalf("recorded-input prior = %+v err=%v, want %+v", prior, err, want)
	}
	if err = tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
		t.Fatalf("reboot-closed predecessor without identity was not released: %v", err)
	}
	if err = fixture.successor.ClaimSession(fixture.req.Provider, prior.ConversationID); err != nil {
		t.Fatalf("successor could not claim the recorded-input session: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(fixture.td.Dir, "provider.ref.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recorded input fabricated an observed provider identity: %v", err)
	}
	requireRebootNoProviderPublication(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)
}

func TestContinuationPriorRefusesFreshPredecessorWithoutIdentity(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{stage: rebootLifetimeClosedPreviousBoot})
	prior, err := tasklifecycle.ContinuationPrior(fixture.td, &fixture.req)
	if !errors.Is(err, os.ErrNotExist) || prior != nil {
		t.Fatalf("fresh predecessor without identity produced prior=%+v err=%v, want not-exist refusal", prior, err)
	}
}

// Only genuine absence of provider.ref.json permits recorded input. Every
// present-but-invalid record or missing prepared dependency stays a refusal.
func TestContinuationPriorNeverFallsBackFromInvalidIdentity(t *testing.T) {
	cases := []struct {
		name   string
		damage func(*testing.T, *rebootFixture)
	}{
		{name: "malformed", damage: func(t *testing.T, fixture *rebootFixture) {
			writeRebootTaskFile(t, fixture, "provider.ref.json", []byte("{"))
		}},
		{name: "mismatched conversation", damage: writeMismatchedRebootIdentity},
		{name: "replaced by directory", damage: func(t *testing.T, fixture *rebootFixture) {
			if err := os.Mkdir(filepath.Join(fixture.td.Dir, "provider.ref.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "replaced by dangling symlink", damage: func(t *testing.T, fixture *rebootFixture) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(fixture.td.Dir, "provider.ref.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing prepared metadata", damage: func(t *testing.T, fixture *rebootFixture) {
			if err := os.Remove(filepath.Join(fixture.td.Dir, "meta.json")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), stage: rebootLifetimeClosedPreviousBoot})
			tc.damage(t, fixture)
			prior, err := tasklifecycle.ContinuationPrior(fixture.td, &fixture.req)
			if err == nil || prior != nil {
				t.Fatalf("invalid identity fell back to recorded input: prior=%+v err=%v", prior, err)
			}
			requireRebootSuccessorBusy(t, fixture)
		})
	}
}

func writeRebootTaskFile(t *testing.T, fixture *rebootFixture, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.td.Dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeMismatchedRebootIdentity writes a schema-valid identity for another
// conversation, which the requested PriorSession must reject.
func writeMismatchedRebootIdentity(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	_, _, spec, metaHash, err := fixture.td.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	record := task.ProviderRefRecord{SchemaVersion: task.SchemaVersion, RootID: fixture.req.RootID, TaskID: fixture.req.TaskID, SpecSHA256: spec, MetaSHA256: metaHash, Provider: fixture.req.Provider, ConversationID: "another-conversation", ObservedAt: "2026-09-14T00:00:00Z"}
	if err = task.ValidateProviderRefRecord(&record); err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	writeRebootTaskFile(t, fixture, "provider.ref.json", data)
}
