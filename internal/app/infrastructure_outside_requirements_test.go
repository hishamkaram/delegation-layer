package app

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestInfrastructureOutsideRequirementsUsesFixedHostWithoutWrites(t *testing.T) {
	directory := supervisorfixture.CanonicalTempDir(t)
	for _, name := range []string{"delegate-run", "pueue", "pueued"} {
		supervisorfixture.WriteExecutable(t, filepath.Join(directory, name), "#!/bin/sh\nexit 0\n")
	}
	host := newInfrastructureEstablishmentHost(filepath.Join(directory, "delegate-run"), supervisor.ExecutableLookup{InitialExecutable: filepath.Join(directory, "pueue"), Environment: []string{}}, infrastructure.InitialOperation{})
	store := newBareInspectionStore(t)
	before := snapshotContinuationSuccessorTestState(t, store.Root)
	if _, err := host.outsideRequirements(t.Context(), store); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("unmanaged selected runner accepted: %v", err)
	}
	if !maps.Equal(before, snapshotContinuationSuccessorTestState(t, store.Root)) {
		t.Fatal("requirements observation changed target")
	}
	if _, err := host.outsideRequirements(t.Context(), nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil store: %v", err)
	}
	var missingContext context.Context // Exercise the invalid input boundary.
	if _, err := host.outsideRequirements(missingContext, store); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil context: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := host.outsideRequirements(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
}
