package app

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestInfrastructureEstablishmentHostCapturesFixedRunnerAndOperation(t *testing.T) {
	initial := infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}
	host := newInfrastructureEstablishmentHost("/accepted/delegate-run", initial)
	initial.Kind = infrastructure.InitialEnsureOwn
	if host.defaultRunner != "/accepted/delegate-run" || host.initial.Kind != infrastructure.InitialMigrateOwn {
		t.Fatal("host changed trusted runner selection or initial operation")
	}
}

func TestInfrastructureEstablishmentHostRejectsInvalidCallBeforeLookup(t *testing.T) {
	host := newInfrastructureEstablishmentHost("/missing/runner", infrastructure.InitialOperation{})
	store := newBareInspectionStore(t)
	var missingContext context.Context // Exercise the invalid input boundary.
	if _, err := host.PrepareOutsideRunner(missingContext, store); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil context: %v", err)
	}
	if _, err := host.PrepareOutsideRunner(t.Context(), nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil store: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	if _, err := host.PrepareOutsideRunner(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context reached runner lookup: %v", err)
	}
	if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
		t.Fatal("invalid call changed target store")
	}
}

func TestInfrastructureEstablishmentHostResolvesInstalledRunnerInPlace(t *testing.T) {
	directory := supervisorfixture.CanonicalTempDir(t)
	runner := filepath.Join(directory, "selected-runner")
	supervisorfixture.WriteExecutable(t, runner, "#!/bin/sh\nexit 0\n")
	alias := filepath.Join(directory, "runner-alias")
	if err := os.Symlink(runner, alias); err != nil {
		t.Fatal(err)
	}
	store := newBareInspectionStore(t)
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	for _, selected := range []string{runner, alias} {
		host := newInfrastructureEstablishmentHost(selected, infrastructure.InitialOperation{})
		actual, err := host.PrepareOutsideRunner(t.Context(), store)
		if err != nil || actual != runner {
			t.Fatalf("installed selection: got=%q err=%v", actual, err)
		}
	}
	if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
		t.Fatal("runner selection staged files or changed target state")
	}
}

func TestInfrastructureEstablishmentHostRejectsUnavailableInstalledRunner(t *testing.T) {
	directory := supervisorfixture.CanonicalTempDir(t)
	nonExecutable := filepath.Join(directory, "non-executable")
	if err := os.WriteFile(nonExecutable, []byte("fixture source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newBareInspectionStore(t)
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	for _, fixture := range []struct {
		path string
		want error
	}{
		{filepath.Join(directory, "absent"), os.ErrNotExist},
		{nonExecutable, pueue.ErrConfiguration},
		{directory, pueue.ErrConfiguration},
	} {
		host := newInfrastructureEstablishmentHost(fixture.path, infrastructure.InitialOperation{})
		if _, err := host.PrepareOutsideRunner(t.Context(), store); !errors.Is(err, fixture.want) {
			t.Fatalf("unavailable selection %q: got=%v want=%v", fixture.path, err, fixture.want)
		}
	}
	if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
		t.Fatal("unavailable selection changed target state")
	}
}
