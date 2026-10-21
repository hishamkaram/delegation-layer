package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// checkpointInventory holds a qualified checkpoint, an expired pending one, a
// removal attempt, the fixture's capture operation and a bare directory.
func checkpointInventory(t *testing.T) (*taskdir.Store, *workspace.Operation, *workspace.Operation) {
	t.Helper()
	store := newCheckpointStore(t)
	qualified := qualifiedOrdinaryCheckpoint(t, store, true)
	pending := openOrdinaryCheckpoint(t, store, strings.Repeat("4", 32), time.Now().Add(-time.Second))
	openCheckpointRemoval(t, store, qualified, strings.Repeat("2", 32), time.Now().Add(time.Hour))
	workspacefixture.MakeBareOperationDirectory(t, store, strings.Repeat("3", 32))
	return store, qualified, pending
}

// listOne reads the single-item page after the cursor.
func listOne(t *testing.T, store *taskdir.Store, after string) (CheckpointDescription, string) {
	t.Helper()
	page, cursor, err := ListCheckpoints(t.Context(), store, after, 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("page after %q = %+v, cursor %q, %v", after, page, cursor, err)
	}
	return page[0], cursor
}

func TestListCheckpointsReadsMetadataOnlyInPages(t *testing.T) {
	store, qualified, pending := checkpointInventory(t)
	first, cursor := listOne(t, store, "")
	if cursor != qualified.Request().OperationID {
		t.Fatalf("first page cursor = %q", cursor)
	}
	if first.State != workspace.CheckpointCheckpointed || !first.Usable || first.Selection != workspace.CheckpointOrdinary || first.SnapshotRef == "" {
		t.Fatalf("qualified checkpoint summary = %+v", first)
	}
	second, cursor := listOne(t, store, cursor)
	if cursor != "" || second.ID != pending.Request().OperationID || second.State != CheckpointPending || second.Usable {
		t.Fatalf("second page = %+v, cursor %q", second, cursor)
	}
	_, err := pending.ReadLifetimeUnlaunchedContext(t.Context())
	if !workspace.OperationAbsent(err) {
		t.Fatalf("list fenced an expired checkpoint: %v", err)
	}
	if _, err = pending.CheckpointReceipt(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("list qualified a checkpoint: %v", err)
	}
	if _, _, err = ListCheckpoints(t.Context(), store, "", 0); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("empty page limit = %v", err)
	}
}

func TestListCheckpointsFaultsEveryLoadErrorAfterPresence(t *testing.T) {
	cases := map[string]func(*testing.T, *taskdir.Store, string){
		"malformed request": func(t *testing.T, store *taskdir.Store, id string) {
			workspacefixture.WriteOperationFile(t, store, id, "request.json", []byte("{"))
		},
		"missing lock": func(t *testing.T, store *taskdir.Store, id string) {
			workspacefixture.RemoveOperationFile(t, store, id, ".operation.lock")
		},
		"identity mismatch": func(t *testing.T, store *taskdir.Store, id string) {
			request := workspacefixture.CheckpointOperationRequest(store, strings.Repeat("6", 32), workspace.OperationCapture, strings.Repeat("b", 32),
				workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("c", 32), SHA256: task.ComputeSHA256([]byte("subject"))}, nil, time.Now().Add(time.Hour))
			data, err := task.MarshalCanonical(request)
			if err != nil {
				t.Fatal(err)
			}
			workspacefixture.WriteOperationFile(t, store, id, "request.json", data)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			store, _, _ := checkpointInventory(t)
			id := strings.Repeat("5", 32)
			if err := openOrdinaryCheckpoint(t, store, id, time.Now().Add(time.Hour)).Close(); err != nil {
				t.Fatal(err)
			}
			corrupt(t, store, id)
			if page, _, err := ListCheckpoints(t.Context(), store, "", 10); err == nil {
				t.Fatalf("list skipped a present but unloadable request: %+v", page)
			}
		})
	}
}

func TestListCheckpointsRejectsForgedCreationIntent(t *testing.T) {
	store, _, _ := checkpointInventory(t)
	selection := ordinaryCheckpointFixtureSelection()
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("9", 32), SHA256: task.ComputeSHA256([]byte("forged"))}
	request := workspacefixture.CheckpointOperationRequest(store, strings.Repeat("5", 32), workspace.OperationCheckpoint, "", subject, &selection, time.Now().Add(time.Hour))
	workspacefixture.OpenCheckpointOperation(t, store, request)
	if _, _, err := ListCheckpoints(t.Context(), store, "", 10); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("forged creation intent = %v, want identity mismatch", err)
	}
}
