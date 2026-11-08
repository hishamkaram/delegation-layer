package tasklifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRemoteReconcileCannotCreateMissingTask(t *testing.T) {
	store := newResourceGateStore(t)
	id := strings.Repeat("c", 32)
	opened := 0
	options := DispatchOptions{OpenStore: func(root string, create bool) (*taskdir.Store, error) {
		opened++
		if create {
			t.Fatal("reconcile requested initialization")
		}
		return taskdir.OpenStore(root)
	}}
	result := ReconcileRemoteContext(t.Context(), DispatchRequest{Root: store.Root, Cwd: t.TempDir(), TaskID: id, Provider: "claude:print", BriefBytes: []byte("brief")}, options)
	if result.Err() == nil || result.RecordPresence() == RecordCreated || opened != 1 {
		t.Fatalf("unexpected reconcile: %+v error %v", result, result.Err())
	}
	if _, err := os.Stat(filepath.Join(store.Root, "tasks", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created missing task: %v", err)
	}
}

func TestRemoteReconcileRejectsBareTaskDirectory(t *testing.T) {
	store := newResourceGateStore(t)
	id := strings.Repeat("d", 32)
	if err := os.MkdirAll(filepath.Join(store.Root, "tasks", id), 0o700); err != nil {
		t.Fatal(err)
	}
	options := DispatchOptions{OpenStore: func(root string, create bool) (*taskdir.Store, error) {
		if create {
			t.Fatal("initialization")
		}
		return taskdir.OpenStore(root)
	}}
	result := ReconcileRemoteContext(t.Context(), DispatchRequest{Root: store.Root, Cwd: t.TempDir(), TaskID: id, Provider: "claude:print", BriefBytes: []byte("brief")}, options)
	if result.Err() == nil {
		t.Fatal("bare directory authorized recovery")
	}
	if _, err := os.Stat(filepath.Join(store.Root, "tasks", id, "task.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created request: %v", err)
	}
}
