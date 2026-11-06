package tasklifecycle

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type routingStoreCleanupError struct {
	store *taskdir.Store
	cause error
}

func (e *routingStoreCleanupError) Error() string                 { return e.cause.Error() }
func (e *routingStoreCleanupError) Unwrap() error                 { return e.cause }
func (e *routingStoreCleanupError) RetainedStore() *taskdir.Store { return e.store }

type routedCleanupProjectionError struct{ owner error }

func (routedCleanupProjectionError) Error() string { return "projected routed cleanup" }

// errors.As requires any and validates the target before delegation.
func (e routedCleanupProjectionError) As(target any) bool { return errors.As(e.owner, target) }

func TestAdmissionCleanupFaultPreservesVirtualHistoricalOwner(t *testing.T) {
	child := &routingStoreCleanupError{cause: errors.New("settled routed cleanup")}
	if !admissionCleanupFault(routedCleanupProjectionError{owner: child}) {
		t.Fatal("virtual view erased concrete historical cleanup failure")
	}
}

func TestRoutedCleanupRetainsActualStore(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	child := &routingStoreCleanupError{store: fixture.store, cause: errors.New("startup attempt cleanup unresolved")}
	cause := errors.Join(errors.New("earlier observation fault"), child)
	store, err := closeAdmissionStore(fixture.store, cause)
	if err != nil || store != fixture.store {
		t.Fatal("routed cleanup lost actual Store")
	}
	if _, err = store.PinRootContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	child.store = nil
	if pendingAdmissionCleanup(cause) || !admissionCleanupFault(cause) {
		t.Fatal("settled child retained custody or erased concrete cleanup fault")
	}
	if store, err = closeAdmissionStore(fixture.store, cause); err != nil || store != nil {
		t.Fatal("settled child prevented actual Store close")
	}
	if _, err = fixture.store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("settled cleanup left Store open")
	}
}
