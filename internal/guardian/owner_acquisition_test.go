package guardian

import (
	"context"
	"errors"
	"testing"
)

func TestGuardianRetainsPartialOwnerBeforeAcquisitionError(t *testing.T) {
	primary := errors.New("acquisition failed")
	release := errors.New("cleanup failed")
	owner := &lifetimeGuardianTestOwner{timeline: &lifetimeGuardianTestTimeline{}, releaseErrors: []error{release}}
	g := &Guardian{options: Options{AcquireOwner: func(context.Context) (Owner, error) { return owner, primary }}}
	observed, err := g.acquireRunOwner(t.Context())
	if observed != owner || g.owner != owner {
		t.Fatal("partial owner discarded")
	}
	if !errors.Is(err, primary) || !errors.Is(err, release) {
		t.Fatalf("acquisition/cleanup cause lost: %v", err)
	}
	if owner.releases != 1 {
		t.Fatalf("cleanup attempts = %d", owner.releases)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
}
