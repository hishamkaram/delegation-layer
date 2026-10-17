package tasklifecycle

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var errInjectedPredicate = errors.New("predicate evaluation failed")

func TestCollectionPendingClassificationRejectsJoinedInfrastructureFault(t *testing.T) {
	if !IsPureCollectionPendingError(errors.Join(task.ErrNoSeal, task.ErrLockBusy)) {
		t.Fatal("pending sentinels were not recognized")
	}
	if IsPureCollectionPendingError(errors.Join(task.ErrNoSeal, errInjectedPredicate)) {
		t.Fatal("infrastructure fault was downgraded to pending")
	}
}

func TestStatusUncertaintyDoesNotHideStorageFault(t *testing.T) {
	if !IsPureSupervisorUncertaintyError(errors.Join(pueue.ErrUnknown, pueue.ErrInFlight)) {
		t.Fatal("pure supervisor uncertainty was not recognized")
	}
	if IsPureSupervisorUncertaintyError(errors.Join(pueue.ErrUnknown, errInjectedPredicate)) {
		t.Fatal("storage fault was downgraded to supervisor uncertainty")
	}
}
