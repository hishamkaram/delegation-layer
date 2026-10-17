package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestPrivateTaskStatusRechecksPhaseInsideObservationLease(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "fresh"
		if cached {
			name = "overlapping-cache"
		}
		t.Run(name, func(t *testing.T) {
			runPrivateTaskStatusPhaseFault(t, cached)
		})
	}
}

func runPrivateTaskStatusPhaseFault(t *testing.T, cached bool) {
	t.Helper()
	fixture, identity := newPreparedPrivateTaskStatus(t)
	replay, err := NewClient(fixture.legacy.Binding(), fixture.legacy.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := replay.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	var cacheBefore []byte
	fixture.legacy.options.taskObservationEntryObserver = func() {
		if cached {
			observation, observeErr := replay.ReconcileStatus(t.Context(), identity)
			if observeErr != nil || !observation.Matched {
				t.Fatalf("prepare overlapping observation: %+v err=%v", observation, observeErr)
			}
			cacheBefore = readPrivateSupervisorFile(t, privateTaskObservationPath(fixture.stateRoot))
		}
		faultPreparedPrivateObservation(t, fixture, "shutdown-requested")
	}
	observation, err := fixture.legacy.ReconcileStatus(t.Context(), identity)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || observation.Matched {
		t.Errorf("shutdown intent became observable after preparation: %+v err=%v", observation, err)
	}
	assertRefusedPrivateStatusCache(t, fixture.stateRoot, cacheBefore)
}

func TestPrivateTaskStatusKeepsPreparedGenerationObservable(t *testing.T) {
	fixture, identity := newPreparedPrivateTaskStatus(t)
	observation, err := fixture.legacy.ReconcileStatus(t.Context(), identity)
	if err != nil || !observation.Matched || observation.NumericTaskID != 42 {
		t.Fatalf("prepared predecessor lost read authority: %+v err=%v", observation, err)
	}
	assertPreparedPrivateObservationIncomplete(t, fixture)
	assertBusyPreparedPrivateObservation(t, fixture)
}

func TestPrivateTaskStatusKeepsPreparedRecoveryAliasObservable(t *testing.T) {
	fixture, identity := newPreparedPrivateTaskStatus(t)
	binding := fixture.legacy.Binding()
	client, _, found, err := recoverPrivateRecoveryBinding(t.Context(), fixture.stateRoot, binding, binding.ClientExecutable, binding.DaemonExecutable, fixture.legacy.options)
	if err != nil || !found || client == nil {
		t.Fatalf("recover prepared alias: found=%t client=%v err=%v", found, client, err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if client.RuntimeBinding().ConfigPath != privateRecoveryConfigPath(fixture.stateRoot) {
		t.Fatal("reader did not use the retained recovery alias")
	}
	observation, err := client.ReconcileStatus(t.Context(), identity)
	if err != nil || !observation.Matched || observation.NumericTaskID != 42 {
		t.Fatalf("prepared alias lost read authority: %+v err=%v", observation, err)
	}
	assertPreparedPrivateObservationIncomplete(t, fixture)
	assertBusyPreparedPrivateObservation(t, fixture)
}

func newPreparedPrivateTaskStatus(t *testing.T) (preparedPrivateObservationFixture, Identity) {
	t.Helper()
	fixture := newBusyPreparedPrivateObservation(t)
	if err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(context.Context) error {
		return EnsurePrivateTaskObservationWithBootstrapLock(fixture.stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	return fixture, writeCompletedPrivateRemovalTask(t, fixture.stateRoot)
}

func assertRefusedPrivateStatusCache(t *testing.T, root string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(privateTaskObservationPath(root))
	if before == nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("refused observation published a cache: %v", err)
		}
		return
	}
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("refused observation replaced the overlapping cache: %v", err)
	}
}
