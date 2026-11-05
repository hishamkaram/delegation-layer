package app

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestInfrastructureEstablishmentHostOwnsCapturedLookup(t *testing.T) {
	environment := []string{"PATH=/accepted/bin", "PROVIDER=accepted\xffbytes", "HOME=/accepted/home"}
	lookup := supervisor.ExecutableLookup{
		InitialExecutable: "/accepted/delegate", Environment: environment,
		AcceptedBootstrapPair: supervisor.BootstrapPair{Client: "/accepted/pueue", Daemon: "/accepted/pueued"},
	}
	expected := slices.Clone(environment)
	host := newInfrastructureEstablishmentHost("/accepted/delegate-run", lookup, infrastructure.InitialOperation{})
	environment[0], lookup.InitialExecutable = "PATH=/changed/bin", "/changed/delegate"
	lookup.AcceptedBootstrapPair.Client = "/changed/pueue"
	if !slices.Equal(host.lookup.Environment, expected) {
		t.Fatal("host borrowed mutable environment")
	}
	if host.lookup.InitialExecutable != "/accepted/delegate" || host.defaultRunner != "/accepted/delegate-run" || host.lookup.AcceptedBootstrapPair.Client != "/accepted/pueue" {
		t.Fatal("host changed trusted selection")
	}
}

func TestInfrastructureEstablishmentHostCapturesNilEnvironmentOnce(t *testing.T) {
	t.Setenv("DELEGATE_TEST_ESTABLISHMENT_CAPTURE", "before")
	host := newInfrastructureEstablishmentHost("", supervisor.ExecutableLookup{}, infrastructure.InitialOperation{})
	expected := os.Environ()
	t.Setenv("DELEGATE_TEST_ESTABLISHMENT_CAPTURE", "after")
	if host.lookup.Environment == nil || !slices.Equal(host.lookup.Environment, expected) {
		t.Fatal("nil environment did not capture original command scope")
	}
	empty := newInfrastructureEstablishmentHost("", supervisor.ExecutableLookup{Environment: []string{}}, infrastructure.InitialOperation{})
	if empty.lookup.Environment == nil || len(empty.lookup.Environment) != 0 {
		t.Fatal("explicit-empty scope fell back to ambient environment")
	}
}

func TestInfrastructureEstablishmentHostRejectsInvalidCallBeforeLookup(t *testing.T) {
	host := newInfrastructureEstablishmentHost("/missing/runner", supervisor.ExecutableLookup{Environment: []string{}}, infrastructure.InitialOperation{})
	store := newBareInspectionStore(t)
	var missingContext context.Context // Exercise the invalid input boundary.
	if _, err := host.PrepareOutsideArtifacts(missingContext, store); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil context: %v", err)
	}
	if _, err := host.PrepareOutsideArtifacts(t.Context(), nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil store: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	if _, err := host.PrepareOutsideArtifacts(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context reached runner lookup: %v", err)
	}
	if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
		t.Fatal("invalid call changed target store")
	}
}

func TestInfrastructureEstablishmentHostRejectsRunnerAliasBeforeTargetWrites(t *testing.T) {
	directory := supervisorfixture.CanonicalTempDir(t)
	runner := filepath.Join(directory, "delegate-run")
	supervisorfixture.WriteExecutable(t, runner, "#!/bin/sh\nexit 0\n")
	alias := filepath.Join(directory, "runner-alias")
	if err := os.Symlink(runner, alias); err != nil {
		t.Fatal(err)
	}
	host := newInfrastructureEstablishmentHost(alias, supervisor.ExecutableLookup{Environment: []string{}}, infrastructure.InitialOperation{})
	store := newBareInspectionStore(t)
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	if _, err := host.PrepareOutsideArtifacts(t.Context(), store); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("runner alias reached copying: %v", err)
	}
	if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
		t.Fatal("runner alias changed target store")
	}
}

// Eligibility belongs to supervisor's selected-file build-info check, not the
// ordinary CLI's running image. These scripts are deliberately not managed Go
// runners; no script or daemon is executed by artifact preparation.
func TestInfrastructureEstablishmentHostDelegatesManagedFileEligibility(t *testing.T) {
	for _, selectedName := range []string{"delegate", "delegate-run"} {
		t.Run(selectedName, func(t *testing.T) {
			directory := supervisorfixture.CanonicalTempDir(t)
			runner := filepath.Join(directory, selectedName)
			for _, name := range []string{selectedName, "pueue", "pueued"} {
				supervisorfixture.WriteExecutable(t, filepath.Join(directory, name), "#!/bin/sh\nexit 0\n")
			}
			host := newInfrastructureEstablishmentHost(runner, supervisor.ExecutableLookup{InitialExecutable: filepath.Join(directory, "delegate"), Environment: []string{}}, infrastructure.InitialOperation{})
			store := newBareInspectionStore(t)
			before := snapshotContinuationSuccessorTestState(t, store.Root)
			_, err := host.PrepareOutsideArtifacts(t.Context(), store)
			if !errors.Is(err, pueue.ErrConfiguration) || !strings.Contains(err.Error(), "candidate is not a managed runner") {
				t.Fatalf("selected file did not reach canonical managed eligibility: %v", err)
			}
			if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
				t.Fatal("unmanaged selected file changed target store")
			}
		})
	}
}
