package control_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/control"
	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestBootstrapCleanupCommandFaultPreservesLifecycleUncertainty(t *testing.T) {
	for _, settled := range []bool{true, false} {
		t.Run(map[bool]string{true: "settled", false: "late"}[settled], func(t *testing.T) {
			root, pending, owner := newBootstrapPolicyFixture(t)
			commandErr := errors.New("status command exited after its observation window")
			if settled {
				transport.CompletePending(pending, transport.CommandResult{Err: commandErr})
			}
			resultErr := control.WithBootstrapLockContext(t.Context(), root, func(context.Context) error {
				return &transport.InFlightError{Pending: pending}
			})
			found := errors.As(resultErr, owner)
			if !tasklifecycle.IsPureSupervisorUncertaintyError(resultErr) {
				t.Fatalf("command outcome changed actual lifecycle uncertainty: %v", resultErr)
			}
			if !found || *owner == nil {
				t.Fatal("completed command diagnostics have no cleanup owner")
			}
			transport.CompletePending(pending, transport.CommandResult{Err: commandErr})
			awaitBootstrapPolicyOwner(t, *owner)
			if !tasklifecycle.IsPureSupervisorUncertaintyError(resultErr) || errors.Is(resultErr, commandErr) {
				t.Fatalf("late outcome changed the same lifecycle error graph: %v", resultErr)
			}
			if !errors.Is((*owner).Err(), commandErr) || (*owner).CleanupPending() {
				t.Fatalf("command diagnostics/custody lost: %v", (*owner).Err())
			}
		})
	}
}

func newBootstrapPolicyFixture(t *testing.T) (string, *transport.Pending, **control.BootstrapCleanupError) {
	t.Helper()
	root, err := os.MkdirTemp("", "delegate-bootstrap-policy-")
	if err != nil {
		t.Fatal(err)
	}
	pending := transport.NewPending(nil, nil)
	var owner *control.BootstrapCleanupError
	t.Cleanup(func() {
		transport.CompletePending(pending, transport.CommandResult{})
		if owner != nil {
			awaitBootstrapPolicyOwner(t, owner)
			closeErr := owner.Close()
			if owner.CleanupPending() {
				t.Errorf("bootstrap policy fixture retained at %s: %v", root, closeErr)
				return
			}
			// Command diagnostics are asserted by the test. A release fault still
			// prevents claiming native closure or removing the owned path.
			if !errors.Is(errors.Unwrap(owner), control.ErrInFlight) {
				t.Errorf("bootstrap policy release fault at %s: %v", root, closeErr)
				return
			}
		}
		if removeErr := os.RemoveAll(root); removeErr != nil {
			t.Error(removeErr)
		}
	})
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, pending, &owner
}

func awaitBootstrapPolicyOwner(t *testing.T, owner *control.BootstrapCleanupError) {
	t.Helper()
	select {
	case <-owner.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap policy cleanup did not complete naturally")
	}
}
