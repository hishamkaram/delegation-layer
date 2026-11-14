package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func workspaceConstraintFixture(t *testing.T, store *Store) (*task.WorkspaceConstraint, string) {
	t.Helper()
	must(t, store.EnableWorkspaceConnectorContext(t.Context(), true, true))
	workspace := filepath.Join(t.TempDir(), "workspace")
	must(t, os.MkdirAll(filepath.Join(workspace, "team", "project"), 0o700))
	identity, err := ObserveProjectContext(t.Context(), workspace)
	must(t, err)
	token, err := ProjectToken(identity)
	must(t, err)
	return &task.WorkspaceConstraint{Token: token, RelativeProject: "team/project"}, identity.Path
}

func TestWorkspaceConnectorInspectionAndEscapes(t *testing.T) {
	store := testStore(t)
	constraint, workspace := workspaceConstraintFixture(t, store)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	observed, err := reader.InspectWorkspaceConnectorContext(t.Context(), constraint)
	must(t, err)
	actual, err := ObserveProjectContext(t.Context(), filepath.Join(workspace, "team", "project"))
	must(t, err)
	expected, err := ProjectToken(actual)
	must(t, err)
	if observed.ProjectToken != expected || !reflect.DeepEqual(observed.WorkspaceConstraint, constraint) {
		t.Fatalf("wrong selection: %+v", observed)
	}
	for _, path := range []string{"../outside", "/absolute", "team/../project", "team/./project", "team\\project", ""} {
		changed := *constraint
		changed.RelativeProject = path
		if _, err = reader.InspectWorkspaceConnectorContext(t.Context(), &changed); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
	must(t, os.Symlink(t.TempDir(), filepath.Join(workspace, "escape")))
	changed := *constraint
	changed.RelativeProject = "escape"
	if _, err = reader.InspectWorkspaceConnectorContext(t.Context(), &changed); err == nil {
		t.Fatal("outside symlink accepted")
	}
	must(t, os.Symlink("team/project", filepath.Join(workspace, "inside")))
	changed.RelativeProject = "inside"
	inside, err := reader.InspectWorkspaceConnectorContext(t.Context(), &changed)
	must(t, err)
	if inside.ProjectToken != expected {
		t.Fatal("inside alias changed physical identity")
	}
}

func replaceWorkspaceKeepingLeaf(t *testing.T, workspace string) {
	t.Helper()
	retained := workspace + "-old"
	must(t, os.Rename(workspace, retained))
	must(t, os.MkdirAll(filepath.Join(workspace, "team"), 0o700))
	must(t, os.Rename(filepath.Join(retained, "team", "project"), filepath.Join(workspace, "team", "project")))
}

func TestWorkspaceConnectorRejectsAncestorReplacementWithSameLeaf(t *testing.T) {
	store := testStore(t)
	constraint, workspace := workspaceConstraintFixture(t, store)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	leaf, err := ObserveProjectContext(t.Context(), filepath.Join(workspace, "team", "project"))
	must(t, err)
	token, err := ProjectToken(leaf)
	must(t, err)
	lease, err := store.RetainWorkspaceConnectorContext(t.Context(), root, token, constraint)
	must(t, err)
	t.Cleanup(func() { must(t, lease.Close()) })
	replaceWorkspaceKeepingLeaf(t, workspace)
	unchanged, err := ObserveProjectContext(t.Context(), leaf.Path)
	must(t, err)
	if unchanged != leaf {
		t.Fatal("fixture did not preserve leaf identity and canonical path")
	}
	if err = lease.Validate(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("retained ancestor check=%v", err)
	}
	if _, err = store.RequireWorkspaceConnectorContext(t.Context(), root, token, leaf.Path, constraint); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("admission ancestor check=%v", err)
	}
}

func TestWorkspaceConnectorGuardRequiresExplicitFeature(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	workspace := t.TempDir()
	identity, err := ObserveProjectContext(t.Context(), workspace)
	must(t, err)
	token, err := ProjectToken(identity)
	must(t, err)
	constraint := &task.WorkspaceConstraint{Token: token, RelativeProject: "."}
	if _, err = store.RequireWorkspaceConnectorContext(t.Context(), root, token, identity.Path, constraint); !errors.Is(err, ErrConnectorDisabled) {
		t.Fatalf("missing feature accepted: %v", err)
	}
	if err = store.EnableWorkspaceConnectorContext(t.Context(), false, true); err == nil {
		t.Fatal("upgrade without attestation")
	}
	must(t, store.EnableWorkspaceConnectorContext(t.Context(), true, true))
	_, err = store.RequireWorkspaceConnectorContext(t.Context(), root, token, identity.Path, constraint)
	must(t, err)
}

func workspacePreparedTask(t *testing.T) (*Store, *TaskDir, *task.WorkspaceConstraint, string) {
	t.Helper()
	store := testStore(t)
	constraint, workspace := workspaceConstraintFixture(t, store)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	must(t, err)
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", inventory.CompletenessReceipt, root)
	must(t, err)
	req, meta, brief := preparedInput(t, store)
	req.CanonicalCwd = filepath.Join(workspace, "team", "project")
	project, err := ObserveProjectContext(t.Context(), req.CanonicalCwd)
	must(t, err)
	req.CanonicalCwd = project.Path
	req.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project, WorkspaceConstraint: constraint.Clone()}
	req.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
	td, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	return store, td, constraint, workspace
}

func TestWorkspaceConnectorCanonicalReceiptSurvivesRemoval(t *testing.T) {
	store, td, constraint, workspace := workspacePreparedTask(t)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	before, err := reader.ReadConnectorReceiptContext(t.Context(), td.TaskID)
	must(t, err)
	if !reflect.DeepEqual(before.WorkspaceConstraint, constraint) {
		t.Fatal("canonical receipt lost workspace constraint")
	}
	must(t, os.RemoveAll(workspace))
	after, err := reader.ReadConnectorReceiptContext(t.Context(), td.TaskID)
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("workspace removal changed receipt")
	}
	guard, err := reader.BorrowConnectorControlContext(t.Context(), before.RootToken, before.ProjectToken)
	must(t, err)
	must(t, guard.Validate(t.Context()))
	if _, err = td.PrepareStart(0); err == nil {
		t.Fatal("removed workspace admitted")
	}
}

func TestWorkspaceConnectorLaunchGateRejectsAncestorReplacement(t *testing.T) {
	_, td, _, workspace := workspacePreparedTask(t)
	permit, err := td.PrepareStart(0)
	must(t, err)
	t.Cleanup(func() { must(t, permit.Release()) })
	must(t, permit.Consume())
	must(t, td.RecordStarted(1))
	replaceWorkspaceKeepingLeaf(t, workspace)
	called := false
	err = permit.WithProjectDirectory(t.Context(), func(*os.File, task.ProjectLaunchContext) error { called = true; return nil })
	if !errors.Is(err, task.ErrIdentityMismatch) || called {
		t.Fatalf("launch gate escaped: called=%v err=%v", called, err)
	}
}

func TestInitializeConnectorStoreCreateOnlyAndConcurrent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			store, err := InitializeConnectorStoreContext(t.Context(), root, predicate.Default(), true)
			if err == nil {
				err = store.Close()
			}
			results <- err
		})
	}
	wg.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, os.ErrExist) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	reader, err := OpenReadOnlyStoreContext(t.Context(), root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	observed, err := reader.InspectConnectorContext(t.Context(), "")
	must(t, err)
	if !slices.Contains(observed.Features, task.WorkspaceConstrainedConnectorFeature) {
		t.Fatalf("missing feature: %+v", observed)
	}
	if _, err = os.Stat(filepath.Join(root, namespaceMutationLockName)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "pueue")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected runtime initialization: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(root, "root.json"))
	must(t, err)
	if _, err = InitializeConnectorStoreContext(t.Context(), root, predicate.Default(), true); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing root adopted: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "root.json"))
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("existing root mutated")
	}
}
