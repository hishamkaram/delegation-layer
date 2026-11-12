package infrastructurehost

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRuntimeRestartAuthorizationBorrowsExactStore(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	calls := 0
	denied := errors.New("fixture root policy denied")
	callback := (Callbacks{RuntimeRestartAuthorization: func(_ context.Context, borrowed *taskdir.Store, from pueue.RuntimeRestartReference, _ task.SupervisorRef, _ []pueue.Job) error {
		calls++
		if borrowed != store || from != "fixture-reference" {
			t.Error("runtime policy was given another Store or correlation")
		}
		return denied
	}}).runtimeRestartAuthorizer(store)
	if err := callback(t.Context(), store.Root, "fixture-reference", task.SupervisorRef{}, nil); !errors.Is(err, denied) || calls != 1 {
		t.Fatalf("borrowed policy lost its exact error/call: calls=%d err=%v", calls, err)
	}
	if err := callback(t.Context(), store.Root+"-foreign", "fixture-reference", task.SupervisorRef{}, nil); !errors.Is(err, infrastructure.ErrIdentity) || calls != 1 {
		t.Fatalf("foreign root reached borrowed policy: calls=%d err=%v", calls, err)
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("policy adapter closed its borrowed Store: %v", err)
	}
}

func TestRuntimeRestartPurposeValidationNeverFabricatesStart(t *testing.T) {
	fixture := newInfrastructurePreparationFixture(t)
	guard := infrastructurePreparationGuard(func(_ context.Context, _ string, callback func() error) error { return callback() })
	_, waits := inertInfrastructurePreparationPorts()
	_, observation, err := infrastructurePreparationOptions(fixture.origin, fixture.artifacts, fixture.selected, guard, waits, testCallbacks())
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("purpose validation refused")
	starts := 0
	purpose := runtimeRestartStartGuard{guard: observation, check: func(context.Context, string) error { return denied }}
	start := func() error { starts++; return nil }
	if err = purpose.GuardDaemonStart(t.Context(), fixture.origin.Root(), start); !errors.Is(err, denied) || starts != 0 || observation.Disposition() != (infrastructure.Disposition{}) {
		t.Fatalf("validation/no-op fabricated Start: starts=%d outcome=%+v err=%v", starts, observation.Disposition(), err)
	}
	purpose.check = func(context.Context, string) error { return nil }
	if err = purpose.GuardDaemonStart(t.Context(), fixture.origin.Root(), start); err != nil || starts != 1 || observation.Disposition().Kind != infrastructure.DispositionStarted {
		t.Fatalf("real successful callback lost its one Start: starts=%d outcome=%+v err=%v", starts, observation.Disposition(), err)
	}
}

type runtimeCustodyAliasError struct{}

func (runtimeCustodyAliasError) Error() string { return "fixture custody alias" }
func (runtimeCustodyAliasError) Is(target error) bool {
	return target == infrastructure.ErrDaemonWaitCustody
}

func TestRuntimeRestartCustodyPreservesAmbiguousAndRetainedFailures(t *testing.T) {
	missing := infrastructure.ErrDaemonWaitCustody
	mixed := errors.Join(missing, errors.New("fixture immutable birth observation failed"))
	retainedStore := &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: &taskdir.Store{}}, cause: missing}
	for _, fixture := range []struct {
		name    string
		err     error
		custody infrastructure.RuntimeRestartCustody
	}{
		{"available", nil, infrastructure.RuntimeRestartCustodyAvailable},
		{"missing exact custody", missing, infrastructure.RuntimeRestartCustodyUnavailable},
		{"mixed missing and birth failure", mixed, infrastructure.RuntimeRestartCustodyUnknown},
		{"wrong root identity", infrastructure.ErrIdentity, infrastructure.RuntimeRestartCustodyUnknown},
		{"sentinel Is alias", runtimeCustodyAliasError{}, infrastructure.RuntimeRestartCustodyUnknown},
		{"retained resident", &retainedPreparationFixtureError{cause: missing}, infrastructure.RuntimeRestartCustodyUnknown},
		{"retained root store", retainedStore, infrastructure.RuntimeRestartCustodyUnknown},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			custody, err := runtimeWaitCustodyResult(fixture.err)
			if custody != fixture.custody {
				t.Fatalf("ambiguous failure became a factual custody result: custody=%s err=%v", custody, err)
			}
			if fixture.custody == infrastructure.RuntimeRestartCustodyUnknown && !reflect.ValueOf(err).Equal(reflect.ValueOf(fixture.err)) {
				t.Fatalf("unknown custody lost its full local cause: got=%v want=%v", err, fixture.err)
			}
			if fixture.custody != infrastructure.RuntimeRestartCustodyUnknown && err != nil {
				t.Fatalf("exact factual custody gained a failure: %v", err)
			}
		})
	}
}
