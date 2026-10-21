package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func operationDirectoryForTest(store *taskdir.Store, id string) string {
	return filepath.Join(store.Root, "workspace-operations", id)
}

func operationEntriesForTest(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func openOperationForTest(t *testing.T, store *taskdir.Store, request OperationRequest) *Operation {
	t.Helper()
	operation, err := OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestOperationRequestPresenceSkipsOnlyOrphanDirectories(t *testing.T) {
	store := newCheckpointStore(t)
	valid := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	present, err := OperationRequestPresent(t.Context(), store, valid.Request().OperationID)
	if err != nil || !present {
		t.Fatalf("valid request presence=%v err=%v", present, err)
	}

	orphan := operationDirectoryForTest(store, testID('2'))
	if err = os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	present, err = OperationRequestPresent(t.Context(), store, testID('2'))
	if err != nil || present {
		t.Fatalf("orphan presence=%v err=%v, want clean absence", present, err)
	}
	if entries := operationEntriesForTest(t, orphan); len(entries) != 0 {
		t.Fatalf("presence probe created records or locks: %v", entries)
	}
}

func TestOperationRequestPresenceKeepsMalformedRequestForLoader(t *testing.T) {
	store := newCheckpointStore(t)
	closeOperation(t, openOperationForTest(t, store, ordinaryCheckpointRequest(store, testID('3'), time.Now().Add(time.Hour))))
	directory := operationDirectoryForTest(store, testID('3'))
	before := operationEntriesForTest(t, directory)
	malformed := []byte("{")
	path := filepath.Join(directory, operationRequestName)
	if err := os.WriteFile(path, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	present, err := OperationRequestPresent(t.Context(), store, testID('3'))
	if err != nil || !present {
		t.Fatalf("malformed request presence=%v err=%v, want present", present, err)
	}
	if !slices.Equal(operationEntriesForTest(t, directory), before) {
		t.Fatal("presence probe changed the operation directory")
	}
	operation, err := LoadOperation(t.Context(), store, testID('3'))
	if err == nil {
		closeOperation(t, operation)
		t.Fatal("unchanged loader accepted a malformed request")
	}
	if task.RecordAbsent(err) {
		t.Fatalf("malformed request was classified absent: %v", err)
	}
	if stored, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(stored, malformed) {
		t.Fatalf("malformed request bytes changed: %q %v", stored, readErr)
	}
}

func TestOperationRequestPresenceReportsDirectoryFaults(t *testing.T) {
	store := newCheckpointStore(t)
	for name, id := range map[string]string{"missing parent": testID('4'), "invalid id": "../escape"} {
		present, err := OperationRequestPresent(t.Context(), store, id)
		if err == nil || present {
			t.Fatalf("%s presence=%v err=%v, want fault", name, present, err)
		}
	}
	openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('5'), time.Now().Add(time.Hour)))
	present, err := OperationRequestPresent(t.Context(), store, testID('6'))
	if err == nil || present {
		t.Fatalf("missing operation directory presence=%v err=%v, want fault", present, err)
	}
}
