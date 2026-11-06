package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// publishedPairRecords are the generation records a refused observation or
// deferral must leave byte-identical.
func publishedPairRecords(stateRoot string) []string {
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	return []string{
		privateSupervisorBindingPath(stateRoot), privateDaemonIdentityPath(base), privateBootstrapRecordPath(stateRoot),
		privateMigrationRecordPath(base), filepath.Join(base, privateMigrationHistoryRecordName),
	}
}

type publishedPairSnapshot map[string][]byte

func snapshotPublishedPairRecords(t *testing.T, stateRoot string) publishedPairSnapshot {
	t.Helper()
	snapshot := publishedPairSnapshot{}
	for _, path := range publishedPairRecords(stateRoot) {
		if data, present := snapshotPrivateTestFile(t, path); present {
			snapshot[path] = data
		}
	}
	return snapshot
}

func (snapshot publishedPairSnapshot) assertUnchanged(t *testing.T, stateRoot string) {
	t.Helper()
	for _, path := range publishedPairRecords(stateRoot) {
		data, present := snapshot[path]
		assertPrivateTestFileUnchanged(t, path, data, present)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 1)
}

func observePublishedTestPair(t *testing.T, stateRoot string) (PublishedPairAvailability, error) {
	t.Helper()
	var availability PublishedPairAvailability
	err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		var observeErr error
		availability, observeErr = ObservePublishedPrivatePairWithLock(ctx, stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
		return observeErr
	})
	return availability, err
}

func privateRecoveryReason(err error) string {
	var recovery *PrivateRecoveryError
	if errors.As(err, &recovery) {
		return recovery.ReasonCode
	}
	return ""
}

// TestObservePublishedPairReportsFactsBeforeMutation observes each clean
// availability fact and each fault of the completed published pair. A launch
// witness for a relocated pair is Incomplete even while the published pair is
// present and healthy, so completeness is decided before presence. Faults
// return no fact. No observation changes a record or starts a daemon.
func TestObservePublishedPairReportsFactsBeforeMutation(t *testing.T) {
	cases := []struct {
		name   string
		inject func(*testing.T, privateSupervisorTestFixture, task.SupervisorRef, task.SupervisorRef)
		want   PublishedPairAvailability
		reason string
	}{
		{name: "ready", want: PublishedPairReady},
		{name: "relocated launch witness", want: PublishedPairIncomplete, inject: func(t *testing.T, _ privateSupervisorTestFixture, original, candidate task.SupervisorRef) {
			writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(filepath.Dir(original.ConfigPath)), privateDaemonIdentityForBinding(candidate))
		}},
		{name: "missing member", want: PublishedPairMemberMissing, inject: func(t *testing.T, _ privateSupervisorTestFixture, original, _ task.SupervisorRef) {
			removePrivateTestPath(t, original.ClientExecutable)
		}},
		{name: "unreadable launch witness", reason: "state_unreadable", inject: func(t *testing.T, _ privateSupervisorTestFixture, original, _ task.SupervisorRef) {
			writePrivateTestFile(t, privateDaemonIdentityPath(filepath.Dir(original.ConfigPath)), "{")
		}},
		{name: "unreadable migration record", reason: "state_unreadable", inject: func(t *testing.T, _ privateSupervisorTestFixture, original, _ task.SupervisorRef) {
			writePrivateTestFile(t, privateMigrationRecordPath(filepath.Dir(original.ConfigPath)), "{")
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, original, candidate := newPrivateReconciliationFixture(t)
			if testCase.inject != nil {
				testCase.inject(t, fixture, original, candidate)
			}
			before := snapshotPublishedPairRecords(t, fixture.stateRoot)
			availability, err := observePublishedTestPair(t, fixture.stateRoot)
			if availability != testCase.want {
				t.Fatalf("availability = %d, want %d (err %v)", availability, testCase.want, err)
			}
			if testCase.want != 0 && err != nil {
				t.Fatalf("clean fact %d carried an error: %v", availability, err)
			}
			if testCase.want == 0 && (!errors.Is(err, ErrPrivateSupervisorMigrationRequired) || privateRecoveryReason(err) != testCase.reason) {
				t.Fatalf("fault = %v (reason %q), want migration-required with reason %q", err, privateRecoveryReason(err), testCase.reason)
			}
			before.assertUnchanged(t, fixture.stateRoot)
		})
	}
	t.Run("not published", func(t *testing.T) {
		fixture := newPrivateSupervisorTestFixture(t)
		makePrivateTestDirectory(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
		if availability, err := observePublishedTestPair(t, fixture.stateRoot); availability != PublishedPairNotPublished || err != nil {
			t.Fatalf("empty root availability = %d, %v", availability, err)
		}
	})
}

func reconcilePublishedTestAttempt(t *testing.T, fixture privateSupervisorTestFixture, candidate task.SupervisorRef) (PublishedPairReconciliation, error) {
	t.Helper()
	var outcome PublishedPairReconciliation
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		var attemptErr error
		outcome, attemptErr = ReconcilePublishedPrivatePairAttemptWithLock(ctx, fixture.stateRoot, func(context.Context) (string, string, error) {
			return candidate.ClientExecutable, candidate.DaemonExecutable, nil
		}, Options{ObservationTimeout: privateTestObservationTimeout})
		return attemptErr
	})
	return outcome, err
}

// TestReconcileAttemptSeparatesCleanDeferralFromReadFault pairs each typed
// reconciliation outcome with the legacy error projection over the same
// state. A readable transition or live endpoint is a clean deferral fact with
// its existing reason; an unreadable record beside a restart intent is a
// state_unreadable error, never transition_pending. Nothing mutates.
func TestReconcileAttemptSeparatesCleanDeferralFromReadFault(t *testing.T) {
	cases := []struct {
		name   string
		inject func(*testing.T, privateSupervisorTestFixture, task.SupervisorRef, task.SupervisorRef)
		want   PublishedPairReconciliation
		reason string
	}{
		{name: "history references pair", want: PublishedPairTransitionPending, reason: "transition_pending", inject: func(t *testing.T, fixture privateSupervisorTestFixture, original, _ task.SupervisorRef) {
			injectPrivateReconciliationFault(t, fixture, original, privateDaemonIdentityForBinding(original), "history-predecessor")
			removePrivateTestPath(t, original.ClientExecutable)
		}},
		{name: "restart intent beside readable migration", want: PublishedPairTransitionPending, reason: "transition_pending", inject: func(t *testing.T, fixture privateSupervisorTestFixture, original, candidate task.SupervisorRef) {
			publishTestPairRestartIntent(t, fixture.stateRoot, original, candidate)
			other := privateDaemonIdentityForBinding(original)
			other.Executable += "-other"
			writePrivateSuccessorTestRecord(t, privateMigrationRecordPath(filepath.Dir(original.ConfigPath)), privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(original), Successor: other})
		}},
		{name: "live endpoint with missing member", want: PublishedPairLiveUnavailable, reason: "published_pair_unavailable_live", inject: func(t *testing.T, _ privateSupervisorTestFixture, original, _ task.SupervisorRef) {
			removePrivateTestPath(t, original.ClientExecutable)
		}},
		{name: "restart intent beside unreadable migration", reason: "state_unreadable", inject: func(t *testing.T, fixture privateSupervisorTestFixture, original, candidate task.SupervisorRef) {
			publishTestPairRestartIntent(t, fixture.stateRoot, original, candidate)
			writePrivateTestFile(t, privateMigrationRecordPath(filepath.Dir(original.ConfigPath)), "{")
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, original, candidate := newPrivateReconciliationFixture(t)
			testCase.inject(t, fixture, original, candidate)
			before := snapshotPublishedPairRecords(t, fixture.stateRoot)
			outcome, err := reconcilePublishedTestAttempt(t, fixture, candidate)
			if outcome != testCase.want || (testCase.want != 0) == (err != nil) {
				t.Fatalf("attempt = %d, %v; want outcome %d", outcome, err, testCase.want)
			}
			if testCase.want == 0 && privateRecoveryReason(err) != testCase.reason {
				t.Fatalf("attempt fault reason = %q, want %q: %v", privateRecoveryReason(err), testCase.reason, err)
			}
			legacy := reconcilePrivateTestPair(t, fixture, candidate)
			if !errors.Is(legacy, ErrPrivateSupervisorMigrationRequired) || privateRecoveryReason(legacy) != testCase.reason {
				t.Fatalf("legacy projection = %v (reason %q), want reason %q", legacy, privateRecoveryReason(legacy), testCase.reason)
			}
			before.assertUnchanged(t, fixture.stateRoot)
		})
	}
}

func publishTestPairRestartIntent(t *testing.T, stateRoot string, original, candidate task.SupervisorRef) {
	t.Helper()
	writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(stateRoot), privateBootstrapRecord{
		SchemaVersion: task.SchemaVersion,
		Restart:       &privatePairRestartIntent{Predecessor: original, Target: candidate},
	})
}

func TestRestartAttemptRefusesIncompleteOrStoppedTargetBeforeSelection(t *testing.T) {
	for _, fault := range []string{"missing publication", "missing witness", "stopped target"} {
		t.Run(fault, func(t *testing.T) {
			fixture, original, candidate := newPrivateReconciliationFixture(t)
			publishTestPairRestartIntent(t, fixture.stateRoot, original, candidate)
			switch fault {
			case "missing publication":
				removePrivateTestPath(t, privateSupervisorBindingPath(fixture.stateRoot))
			case "missing witness":
				removePrivateTestPath(t, privateDaemonIdentityPath(filepath.Dir(original.ConfigPath)))
			case "stopped target":
				removePrivateTestPath(t, filepath.Join(filepath.Dir(original.ConfigPath), "ready"))
			}
			before := snapshotPublishedPairRecords(t, fixture.stateRoot)
			if availability, err := observePublishedTestPair(t, fixture.stateRoot); err != nil || availability != PublishedPairIncomplete {
				t.Fatalf("pending restart availability = %d, %v; want incomplete", availability, err)
			}
			selected := false
			err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
				outcome, attemptErr := ReconcilePublishedPrivatePairAttemptWithLock(ctx, fixture.stateRoot, func(context.Context) (string, string, error) {
					selected = true
					return "", "", os.ErrPermission
				}, Options{ObservationTimeout: privateTestObservationTimeout})
				return errors.Join(attemptErr, outcome.ProjectedError())
			})
			if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || selected {
				t.Fatalf("incomplete restart = %v, selected=%t; want refusal before selection", err, selected)
			}
			if fault == "stopped target" && privateRecoveryReason(err) != "transition_pending" {
				t.Fatalf("stopped exact target reason = %q, want transition_pending: %v", privateRecoveryReason(err), err)
			}
			before.assertUnchanged(t, fixture.stateRoot)
		})
	}
}

func TestReconciliationPreservesSelectorFaults(t *testing.T) {
	for _, selectionErr := range []error{os.ErrPermission, context.Canceled, ErrCompatiblePairUnavailable} {
		t.Run(selectionErr.Error(), func(t *testing.T) {
			fixture, original, _ := newPrivateReconciliationFixture(t)
			removePrivateTestPath(t, original.ClientExecutable)
			before := snapshotPublishedPairRecords(t, fixture.stateRoot)
			err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
				outcome, attemptErr := ReconcilePublishedPrivatePairAttemptWithLock(ctx, fixture.stateRoot, func(context.Context) (string, string, error) {
					return "", "", selectionErr
				}, Options{})
				if outcome != 0 {
					t.Errorf("selector fault returned reconciliation outcome %d", outcome)
				}
				return attemptErr
			})
			if !errors.Is(err, selectionErr) {
				t.Fatalf("selector error = %v, want %v", err, selectionErr)
			}
			before.assertUnchanged(t, fixture.stateRoot)
		})
	}
}

func TestRestartPublicationRefusesNewTargetBeforeMatchingReplay(t *testing.T) {
	fixture, original, target := newPrivateReconciliationFixture(t)
	publishTestPairRestartIntent(t, fixture.stateRoot, original, target)
	writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(fixture.stateRoot), privateIdentityPublicationRecord{
		SchemaVersion: task.SchemaVersion, Binding: target,
	})
	other := target
	other.ClientExecutable += "-other"
	before := snapshotPublishedPairRecords(t, fixture.stateRoot)
	pendingBefore := readPrivateSupervisorFile(t, privateIdentityPublicationPath(fixture.stateRoot))
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(context.Context) error {
		return publishPrivateDaemonIdentity(filepath.Dir(target.ConfigPath), other)
	})
	if !errors.Is(err, ErrBinding) || privateRecoveryReason(err) != "identity_conflict" {
		t.Fatalf("conflicting new publisher = %v; want identity_conflict", err)
	}
	before.assertUnchanged(t, fixture.stateRoot)
	assertPrivateTestFileUnchanged(t, privateIdentityPublicationPath(fixture.stateRoot), pendingBefore, true)
}

func writePrivateTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSettlePublishedPairReconciliationMasksOutcomeOnCloseFault proves the
// release rule of every attempt client: a close fault discards the outcome,
// and the projected deferral reason stays beside the fault in the error.
func TestSettlePublishedPairReconciliationMasksOutcomeOnCloseFault(t *testing.T) {
	closeErr := errors.New("attempt client close failed")
	for _, outcome := range []PublishedPairReconciliation{PublishedPairReconciled, PublishedPairTransitionPending, PublishedPairLiveUnavailable} {
		if got, err := settlePublishedPairReconciliation(outcome, nil, nil); got != outcome || err != nil {
			t.Fatalf("clean release changed outcome %d: %d, %v", outcome, got, err)
		}
		got, err := settlePublishedPairReconciliation(outcome, nil, closeErr)
		if got != 0 || !errors.Is(err, closeErr) {
			t.Fatalf("close fault kept outcome %d: %d, %v", outcome, got, err)
		}
		if projected := outcome.ProjectedError(); projected != nil && privateRecoveryReason(err) != privateRecoveryReason(projected) {
			t.Fatalf("close fault lost reason %q: %v", privateRecoveryReason(projected), err)
		}
	}
	operational := errors.New("restart failed")
	if got, err := settlePublishedPairReconciliation(0, operational, closeErr); got != 0 || !errors.Is(err, operational) || !errors.Is(err, closeErr) {
		t.Fatalf("operational and close faults = %d, %v", got, err)
	}
}
