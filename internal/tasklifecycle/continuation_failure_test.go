package tasklifecycle

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestContinuationFailurePreservesCauseGraphAndSpecificStep(t *testing.T) {
	privateCause := &fs.PathError{Op: "open", Path: "/private/session-token", Err: fs.ErrPermission}
	closeFault := errors.New("private cleanup fault")
	original := errors.Join(privateCause, closeFault)
	specific := withContinuationFailure(ContinuationPredecessorSessionHandoffFailed, original)
	wrapped := withContinuationFailure(ContinuationPredecessorReconciliationFailed, errors.Join(specific, context.Canceled))
	var pathCause *fs.PathError
	if ContinuationReason(wrapped) != "predecessor_session_handoff_failed" ||
		!errors.Is(wrapped, privateCause) || !errors.Is(wrapped, closeFault) || !errors.Is(wrapped, context.Canceled) ||
		!errors.As(wrapped, &pathCause) || pathCause.Path != privateCause.Path {
		t.Fatalf("diagnostic changed source step or cause graph: %v", wrapped)
	}
	if specific.Error() != original.Error() || withContinuationFailure(ContinuationValidationFailed, nil) != nil || ContinuationReason(closeFault) != "" {
		t.Fatal("diagnostic changed error detail, success, or unclassified errors")
	}
}

func TestContinuationFailureSourceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		invoke func(*testing.T, teamDispatchFixture) error
		reason string
		cause  error
	}{
		{name: "missing predecessor records", reason: "predecessor_records_unavailable", cause: os.ErrNotExist, invoke: func(_ *testing.T, f teamDispatchFixture) error {
			_, _, err := ContinuationOptionsForTask(f.store, DispatchOptions{}, strings.Repeat("9", 32))
			return err
		}},
		{name: "missing profile composition", reason: "continuation_profiles_unavailable", cause: task.ErrEvidenceFault, invoke: func(_ *testing.T, f teamDispatchFixture) error {
			_, _, err := ContinuationOptionsForTask(f.store, DispatchOptions{}, f.request.TaskID)
			return err
		}},
		{name: "missing observed and recorded session", reason: "predecessor_session_unavailable", cause: os.ErrNotExist, invoke: func(t *testing.T, f teamDispatchFixture) error {
			_, err := ResolvePredecessorContext(t.Context(), f.store, PredecessorOptions{}, *f.request, f.request.TaskID)
			return err
		}},
		{name: "canceled predecessor observation", reason: "predecessor_observation_failed", cause: context.Canceled, invoke: func(t *testing.T, f teamDispatchFixture) error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return PredecessorTerminatedContext(ctx, f.store.Root, f.td, f.request, SupervisorOptions{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTeamDispatchFixture(t)
			before, err := fixture.store.TaskIDs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			failure := tc.invoke(t, fixture)
			if ContinuationReason(failure) != ContinuationFailureReason(tc.reason) || !errors.Is(failure, tc.cause) {
				t.Fatalf("failure = %v reason %q, want %q with original cause %v", failure, ContinuationReason(failure), tc.reason, tc.cause)
			}
			after, err := fixture.store.TaskIDs(t.Context())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal created a successor: %v -> %v (%v)", before, after, err)
			}
		})
	}
}

func TestContinuationFailureDistinguishesLeaseObservationFromBusy(t *testing.T) {
	// The canonical lease observer rejects a missing handle; it does not prove
	// contention and must never produce a busy diagnostic for that fault.
	err := requireRunnerLeaseRelease(nil)
	if ContinuationReason(err) != "predecessor_runner_lease_observation_failed" || !errors.Is(err, task.ErrInvalidPermit) || errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("invalid lease observation became busy: %v (%q)", err, ContinuationReason(err))
	}
}

func TestContinuationFailureKeepsTerminalHandoffAuthority(t *testing.T) {
	store := newResourceGateStore(t)
	prior := &task.PriorSession{Provider: "fixture:test", ConversationID: "recorded-session", PredecessorTaskID: strings.Repeat("d", 32)}
	td, request := testsupport.NewAppTaskInStore(t, store, strings.Repeat("c", 32), true, prior)
	if err := td.ClaimSession(request.Provider, prior.ConversationID); err != nil {
		t.Fatal(err)
	}
	inspection, err := td.InspectContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = releaseTerminalPredecessor(t.Context(), td, request, inspection)
	if ContinuationReason(err) != "predecessor_outcome_unavailable" || !errors.Is(err, errPredecessorOutcomeUnavailable) {
		t.Fatalf("unaccepted result allowed handoff: %v (%q)", err, ContinuationReason(err))
	}
	if _, cleanup, collectErr := td.Collect(task.FixturePredicateRef()); collectErr != nil || cleanup != nil {
		t.Fatalf("accepting fixture result: %v", errors.Join(collectErr, cleanup))
	}
	inspection, err = td.InspectContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = releaseTerminalPredecessor(ctx, td, request, inspection)
	if ContinuationReason(err) != "predecessor_session_handoff_failed" || !errors.Is(err, context.Canceled) {
		t.Fatalf("handoff lost original cancellation: %v (%q)", err, ContinuationReason(err))
	}
	release := filepath.Join(store.Root, "sessions", task.ComputeSHA256([]byte(request.Provider+":"+prior.ConversationID)), request.TaskID+".release.json")
	if _, err = os.Stat(release); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused handoff published release: %v", err)
	}
	if err = releaseTerminalPredecessor(t.Context(), td, request, inspection); err != nil {
		t.Fatalf("diagnostic altered a subsequent authorized handoff: %v", err)
	}
	if _, err = os.Stat(release); err != nil {
		t.Fatalf("authorized handoff did not publish release: %v", err)
	}
}
