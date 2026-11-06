package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newOwnerStopEmptyReader(t *testing.T) *taskdir.ReadOnlyStore {
	t.Helper()
	root := filepath.Dir(leasePath(t))
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}

func TestOwnerStopClientNoIntentDoesNotMeanNoOwner(t *testing.T) {
	for _, residue := range []string{"empty lease", "busy lease", "malformed lease"} {
		t.Run(residue, func(t *testing.T) {
			reader := newOwnerStopEmptyReader(t)
			createOwnerStopResidue(t, reader.Root(), residue)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := StopOwnerServing(ctx, reader)
			if err == nil || result.State != OwnerStopUnknown || result.ProcessClosure != OwnerProcessUnknown {
				t.Fatalf("unselected birth/owner residue became not-initialized: %+v %v", result, err)
			}
		})
	}
}

func createOwnerStopResidue(t *testing.T, root, residue string) {
	t.Helper()
	lease := filepath.Join(root, instanceLeaseName)
	switch residue {
	case "empty lease":
		if err := os.WriteFile(lease, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	case "busy lease":
		acquire(t, lease)
	case "malformed lease":
		if err := os.Mkdir(lease, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOwnerStopClientContextRecordsAloneDoNotClaimBirth(t *testing.T) {
	reader := newOwnerStopEmptyReader(t)
	directory := filepath.Join(reader.Root(), "infrastructure")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	contextData := []byte("{\"future_context\":true}\n")
	name, err := contextName(LaunchContextReference{SHA256: task.ComputeSHA256(contextData)})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name), contextData, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := StopOwnerServing(ctx, reader)
	if err != nil || result.State != OwnerNotInitialized || result.ProcessClosure != OwnerProcessUnknown {
		t.Fatalf("context data was treated as a birth or process-closure claim: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(reader.Root(), instanceLeaseName)); !os.IsNotExist(err) {
		t.Fatalf("no-intent observation created an instance lease: %v", err)
	}
}
