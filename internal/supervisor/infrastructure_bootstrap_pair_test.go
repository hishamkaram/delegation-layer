package supervisor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestInfrastructureBootstrapPairConflictPreservesInterruptedInventory(t *testing.T) {
	store := newGenerationStore(t)
	client, daemon := writeSavedSupervisorPair(t)
	if err := PrepareStateSupervisorPair(t.Context(), store.Root, client, daemon); err != nil {
		t.Fatal(err)
	}
	inventoryPath := filepath.Join(store.Root, ".supervisor", stateSupervisorPairRecordName)
	inventory, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	configPath := pueue.PrivateConfigPath(store.Root)
	config := []byte("preexisting config must remain untouched\n")
	if err = os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := ExecutableLookup{
		InitialExecutable: filepath.Join(store.Root, "accepted", "pueue"), Environment: []string{},
		AcceptedBootstrapPair: BootstrapPair{
			Client: filepath.Join(store.Root, "accepted", "pueue"),
			Daemon: filepath.Join(store.Root, "accepted", "pueued"),
		},
	}
	// This is the canonical no-published-generation branch. It owns the same
	// transition/bootstrap locks as the production preparation loop. Deliberately
	// omit native authority: a mismatch must refuse before any preparation/Start.
	acquired, err := pueue.TryWithPrivateTransition(t.Context(), store.Root, func() error {
		return pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
			client, startErr := bootstrapPrivateAdmission(ctx, lookup, store, pueue.Options{}, true)
			return ClosePrivateAdmissionTransition(client, startErr)
		})
	})
	if !acquired || !errors.Is(err, ErrIndependentSupervisorSetupRequired) {
		t.Fatalf("mismatched accepted pair was not refused: acquired=%t err=%v", acquired, err)
	}
	for _, snapshot := range []struct {
		path string
		data []byte
	}{{inventoryPath, inventory}, {configPath, config}} {
		after, readErr := os.ReadFile(snapshot.path)
		if readErr != nil || !bytes.Equal(after, snapshot.data) {
			t.Fatalf("bootstrap conflict changed existing state: %v", readErr)
		}
	}
	if _, found, readErr := pueue.ReadPrivateSupervisorBinding(store.Root); readErr != nil || found {
		t.Fatalf("bootstrap conflict published a binding: found=%t err=%v", found, readErr)
	}
}
