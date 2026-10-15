package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestControlRecordNamesUsePinnedRoot(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInspectionGroup(true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	_, err = control.Put("original.json", controlTestRecord{Request: "original"})
	must(t, err)
	must(t, os.Rename(store.Root, store.Root+"-retained"))
	must(t, os.MkdirAll(filepath.Join(store.Root, "inspection-group"), 0o700))
	must(t, os.WriteFile(filepath.Join(store.Root, "inspection-group", "replacement.json"), []byte("{}"), 0o600))
	names, err := control.RecordNamesContext(t.Context())
	must(t, err)
	if !slices.Contains(names, "original.json") || slices.Contains(names, "replacement.json") {
		t.Fatalf("record enumeration escaped its pinned root: %v", names)
	}
}

func TestControlRecordNamesRejectCanceledContext(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInspectionGroup(true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = control.RecordNamesContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("record enumeration lost cancellation: %v", err)
	}
}
