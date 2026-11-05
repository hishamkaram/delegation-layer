package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestWorkspaceResourceIDsAbsentParentCreatesNothing(t *testing.T) {
	fixture := newExistingControlFixture(t)
	before := existingControlTree(t, fixture.store.Root)
	ids, err := fixture.store.WorkspaceResourceIDs(t.Context())
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("absent physical candidates: %v %v", ids, err)
	}
	if after := existingControlTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("listing initialized workspace storage")
	}
	requireMaintenanceReleased(t, fixture.store)
}

func TestWorkspaceResourceIDsIncludesPartialCandidatesWithoutRecordReads(t *testing.T) {
	fixture := newExistingControlFixture(t)
	id := strings.Repeat("a", 32)
	control, err := fixture.store.OpenWorkspace(t.Context(), id, true)
	if control != nil {
		fixture.controls = append(fixture.controls, control)
	}
	must(t, err)
	must(t, os.WriteFile(filepath.Join(fixture.store.Root, "workspaces", id, "request.json"), []byte("invalid request bytes"), 0o600))
	orphan := strings.Repeat("b", 32)
	must(t, os.Mkdir(filepath.Join(fixture.store.Root, "workspaces", orphan), 0o700))
	before := existingControlTree(t, fixture.store.Root)
	ids, err := fixture.store.WorkspaceResourceIDs(t.Context())
	if err != nil || !slices.Equal(ids, []string{id, orphan}) {
		t.Fatalf("physical workspace candidates: %v %v", ids, err)
	}
	if after := existingControlTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("listing read/repaired candidate record state")
	}
}

func TestWorkspaceResourceIDsRefusesMalformedUnsafeAndSymlinkCandidates(t *testing.T) {
	for _, kind := range []string{"malformed", "symlink", "mode", "file"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newExistingControlFixture(t)
			parent := filepath.Join(fixture.store.Root, "workspaces")
			must(t, os.Mkdir(parent, 0o700))
			candidate := filepath.Join(parent, strings.Repeat("a", 32))
			switch kind {
			case "malformed":
				must(t, os.Mkdir(filepath.Join(parent, "not-a-resource"), 0o700))
			case "symlink":
				must(t, os.Symlink("..", candidate))
			case "mode":
				must(t, os.Mkdir(candidate, 0o755))
			case "file":
				must(t, os.WriteFile(candidate, nil, 0o600))
			}
			ids, err := fixture.store.WorkspaceResourceIDs(t.Context())
			if ids != nil || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("unsafe candidate hidden: ids=%v error=%v", ids, err)
			}
			requireMaintenanceReleased(t, fixture.store)
		})
	}
}

func TestWorkspaceResourceIDsHonorsCancellationAndClosedStore(t *testing.T) {
	fixture := newExistingControlFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ids, err := fixture.store.WorkspaceResourceIDs(ctx); ids != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled candidates: %v %v", ids, err)
	}
	must(t, fixture.store.Close())
	if ids, err := fixture.store.WorkspaceResourceIDs(t.Context()); ids != nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed candidates: %v %v", ids, err)
	}
}
