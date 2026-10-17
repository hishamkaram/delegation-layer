package control

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestBindingPublicationIncompleteFailureIsNotPremutationRefusal(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	path := privateSupervisorBindingPath(fixture.stateRoot)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return fixture.legacy.EnsurePrivateSupervisorBindingWithLock(ctx, fixture.stateRoot)
	})
	published, found, readErr := loadPrivateSupervisorBinding(fixture.stateRoot)
	if readErr != nil || !found || published != fixture.legacy.Binding() {
		t.Fatalf("fixture did not reach actual binding publication: found=%t err=%v published=%+v", found, readErr, published)
	}
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || errors.Is(err, ErrBinding) {
		t.Fatalf("post-publication failure changed its original contract: %v", err)
	}
	if errors.Is(err, ErrPrivateSupervisorTransitionIncomplete) {
		t.Errorf("post-publication failure advertised a pre-mutation refusal: %v", err)
	}
}

func TestPrivateCompletionCannotSucceedWithoutRefusalCause(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	if err := ensurePrivateSupervisorMigrationComplete(fixture.stateRoot, fixture.legacy.Binding(), nil); err == nil {
		t.Fatal("a missing refusal cause turned an incomplete lifecycle into success")
	}
}
