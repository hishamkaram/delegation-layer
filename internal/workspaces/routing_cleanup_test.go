package workspaces

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

type retainedRoutingStoreError struct {
	store *taskdir.Store
	cause error
}

func (e *retainedRoutingStoreError) Error() string                 { return e.cause.Error() }
func (e *retainedRoutingStoreError) Unwrap() error                 { return e.cause }
func (e *retainedRoutingStoreError) RetainedStore() *taskdir.Store { return e.store }

type routedCleanupProjectionError struct{ owner error }

func (routedCleanupProjectionError) Error() string { return "projected routed cleanup" }

// errors.As requires any and validates the target before delegation.
func (e routedCleanupProjectionError) As(target any) bool { return errors.As(e.owner, target) }

func TestAdmissionRetainsRoutedChildrenUntilTheirOwnersClose(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct children", true: "virtual children"}[projected], func(t *testing.T) {
			exerciseRoutedAdmissionCleanup(t, projected)
		})
	}
}

func exerciseRoutedAdmissionCleanup(t *testing.T, projected bool) {
	t.Helper()
	store, resource := workspacefixture.NewResource(t)
	admission := &Admission{resource: resource}
	first := &retainedRoutingStoreError{store: store, cause: errors.New("first routed cleanup unresolved")}
	second := &retainedRoutingStoreError{store: store, cause: errors.New("second routed cleanup unresolved")}
	t.Cleanup(func() {
		first.store, second.store = nil, nil
		if closeErr := admission.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	cause := errors.Join(first, second)
	if projected {
		cause = errors.Join(routedCleanupProjectionError{owner: first}, routedCleanupProjectionError{owner: second})
	}
	finished := admission.FinishContext(t.Context(), store, cause, false, nil)
	if finished.CloseError == nil || !admission.CleanupPending() {
		t.Fatal("routed child caused early admission release")
	}
	first.store = nil
	if closeErr := admission.Close(); closeErr == nil {
		t.Fatal("first settled child hid second routed owner")
	}
	second.store = nil
	if err := admission.Close(); err != nil || admission.CleanupPending() {
		t.Fatal("settled routed owners prevented parent cleanup")
	}
}
