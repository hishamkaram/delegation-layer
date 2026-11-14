package taskdir

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProjectTokenPreservesLegacyAndSeparatesPathVersion(t *testing.T) {
	legacy := task.DirectoryIdentity{Path: "/project", Device: 7, Inode: 19}
	token, err := ProjectToken(legacy)
	must(t, err)
	want := base64.RawURLEncoding.EncodeToString([]byte("{\"version\":1,\"project\":{\"path\":\"/project\",\"device\":7,\"inode\":19}}\n"))
	if token != want {
		t.Fatalf("legacy token bytes changed: %s", token)
	}
	path := task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: "/project"}
	token, err = ProjectToken(path)
	must(t, err)
	want = base64.RawURLEncoding.EncodeToString([]byte("{\"version\":2,\"project\":{\"path\":\"/project\"}}\n"))
	if token != want {
		t.Fatalf("path token lacks explicit v2 path authority: %s", token)
	}
	decoded, err := parseProjectToken(token)
	must(t, err)
	if decoded != path {
		t.Fatalf("decoded binding=%+v, want %+v", decoded, path)
	}
	for _, data := range []string{
		"{\"version\":1,\"project\":{\"path\":\"/project\",\"device\":0,\"inode\":0,\"kind\":\"path-v1\"}}\n",
		"{\"version\":2,\"project\":{\"path\":\"/project\",\"device\":7,\"inode\":19}}\n",
		"{\"version\":2,\"project\":{\"path\":\"/project\",\"device\":7,\"inode\":19,\"kind\":\"path-v1\"}}\n",
	} {
		if _, err = parseProjectToken(base64.RawURLEncoding.EncodeToString([]byte(data))); err == nil {
			t.Fatalf("accepted mixed token: %s", data)
		}
	}
}

func pathWorkspaceFixture(t *testing.T, store *Store) (*task.WorkspaceConstraint, task.DirectoryIdentity) {
	t.Helper()
	constraint, workspace := workspaceConstraintFixture(t, store)
	ancestor, err := ObservePathProjectContext(t.Context(), workspace)
	must(t, err)
	constraint.Token, err = ProjectToken(ancestor)
	must(t, err)
	leaf, err := ObservePathProjectContext(t.Context(), filepath.Join(workspace, "team", "project"))
	must(t, err)
	return constraint, leaf
}

func replacePathDirectory(t *testing.T, path string) {
	t.Helper()
	// Keep replacements inside the original test-owned parent so its cleanup
	// follows closure of the Store and all retained directory leases.
	retained, err := os.MkdirTemp(filepath.Dir(path), ".retained-project-")
	must(t, err)
	must(t, os.Rename(path, filepath.Join(retained, "directory")))
	must(t, os.MkdirAll(path, 0o700))
}

func TestPathWorkspaceApprovalSurvivesAncestorAndLeafReplacement(t *testing.T) {
	store := testStore(t)
	constraint, leaf := pathWorkspaceFixture(t, store)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	token, err := ProjectToken(leaf)
	must(t, err)
	lease, err := store.RetainWorkspaceConnectorContext(t.Context(), root, token, constraint)
	must(t, err)
	t.Cleanup(func() { must(t, lease.Close()) })
	ancestor, err := parseProjectToken(constraint.Token)
	must(t, err)
	replacePathDirectory(t, ancestor.Path)
	// Missing paths do not invalidate an operation that already owns its FDs.
	must(t, lease.Validate(t.Context()))
	must(t, os.MkdirAll(leaf.Path, 0o700))
	for _, path := range []string{ancestor.Path, leaf.Path} {
		if path == leaf.Path {
			replacePathDirectory(t, leaf.Path)
		}
		binding, requireErr := store.RequireWorkspaceConnectorContext(t.Context(), root, token, leaf.Path, constraint)
		must(t, requireErr)
		if binding.Project != leaf || !reflect.DeepEqual(binding.WorkspaceConstraint, constraint) {
			t.Fatal("replacement changed logical approval")
		}
		resolved, resolveErr := ResolveProjectTokenContext(t.Context(), token)
		must(t, resolveErr)
		if resolved != leaf {
			t.Fatal("replacement changed the saved token binding")
		}
		must(t, lease.Validate(t.Context()))
	}
}

func pathPreparedTask(t *testing.T) (*Store, *TaskDir, *task.ConnectorContext) {
	t.Helper()
	store := testStore(t)
	constraint, leaf := pathWorkspaceFixture(t, store)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	request, meta, brief := preparedInput(t, store)
	request.CanonicalCwd = leaf.Path
	request.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: leaf, WorkspaceConstraint: constraint}
	directory, err := store.CreateTaskContext(t.Context(), request.TaskID, request, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, directory.Close()) })
	return store, directory, request.ConnectorContext.Clone()
}

func TestPathPreparedStartReacquiresCurrentSelectionAndResultSurvivesRemoval(t *testing.T) {
	_, directory, binding := pathPreparedTask(t)
	before, err := os.ReadFile(filepath.Join(directory.Dir, "task.json"))
	must(t, err)
	ancestor, err := parseProjectToken(binding.WorkspaceConstraint.Token)
	must(t, err)
	replacePathDirectory(t, ancestor.Path)
	must(t, os.MkdirAll(binding.Project.Path, 0o700))
	permit, err := directory.PrepareStart(0)
	must(t, err)
	t.Cleanup(func() { must(t, permit.Release()) })
	must(t, permit.Consume())
	must(t, directory.RecordStarted(1))
	preparedObservation, err := ObserveProjectContext(t.Context(), binding.Project.Path)
	must(t, err)
	replacePathDirectory(t, binding.Project.Path)
	current, err := ObserveProjectContext(t.Context(), binding.Project.Path)
	must(t, err)
	if current == preparedObservation {
		t.Fatal("fixture did not replace the prepared physical directory")
	}
	must(t, permit.WithProjectDirectory(t.Context(), func(file *os.File, launch task.ProjectLaunchContext) error {
		if !reflect.DeepEqual(launch.Binding, *binding) || launch.Observed != current {
			t.Errorf("launch context=%+v; saved=%+v current=%+v", launch, binding, current)
		}
		held, observeErr := heldDirectoryIdentity(file, current.Path)
		if observeErr != nil {
			return observeErr
		}
		if held != current {
			t.Error("launch borrowed a historical directory")
		}
		launch.Binding.WorkspaceConstraint.RelativeProject = "changed"
		return nil
	}))
	must(t, os.RemoveAll(ancestor.Path))
	must(t, permit.RunnerLease().ValidateProject(t.Context()))
	_, err = directory.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	after, err := os.ReadFile(filepath.Join(directory.Dir, "task.json"))
	must(t, err)
	if string(after) != string(before) {
		t.Fatal("replacement or launch callback rewrote the immutable task")
	}
}

func TestPathPreparedStartMissingSelectionCreatesNoStartEvidence(t *testing.T) {
	_, directory, binding := pathPreparedTask(t)
	before, err := os.ReadFile(filepath.Join(directory.Dir, "task.json"))
	must(t, err)
	must(t, os.Remove(binding.Project.Path))
	permit, err := directory.PrepareStart(0)
	if permit != nil {
		must(t, permit.Release())
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path did not refuse preparation: %v", err)
	}
	for _, name := range []string{"provider.start", "provider.started.json", "provider.exit", "outcome.json", "raw"} {
		if _, statErr := os.Stat(filepath.Join(directory.Dir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed selection created %s: %v", name, statErr)
		}
	}
	after, err := os.ReadFile(filepath.Join(directory.Dir, "task.json"))
	must(t, err)
	if string(after) != string(before) {
		t.Fatal("failed selection rewrote the immutable task")
	}
}

func TestPathImmutableRequestRefusesEquivalentSelectionRewrite(t *testing.T) {
	store, directory, binding := pathPreparedTask(t)
	ancestor, err := parseProjectToken(binding.WorkspaceConstraint.Token)
	must(t, err)
	must(t, os.Symlink(filepath.Join("team", "project"), filepath.Join(ancestor.Path, "alias")))
	request, meta, err := directory.PreparedRecords()
	must(t, err)
	brief, err := directory.loadAndValidatePreparedBrief()
	must(t, err)
	request.ConnectorContext.WorkspaceConstraint.RelativeProject = "alias"
	token, err := ProjectToken(binding.Project)
	must(t, err)
	// Both selections resolve to the same approved canonical leaf, but the saved
	// immutable request is compared exactly rather than by path observation.
	_, err = store.RequireWorkspaceConnectorContext(t.Context(), binding.RootToken, token, binding.Project.Path, request.ConnectorContext.WorkspaceConstraint)
	must(t, err)
	changed, err := store.CreateTaskContext(t.Context(), request.TaskID, request, brief, meta)
	if changed != nil {
		must(t, changed.Close())
	}
	if !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("equivalent path selection rewrote immutable request: %v", err)
	}
}

func TestPathProjectDirectoryRefusesMissingMixedAndEscapingSelections(t *testing.T) {
	store := testStore(t)
	constraint, leaf := pathWorkspaceFixture(t, store)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	binding := task.ConnectorContext{RootToken: root, Project: leaf, WorkspaceConstraint: constraint}
	ancestor, err := parseProjectToken(constraint.Token)
	must(t, err)
	physical, err := ObserveProjectContext(t.Context(), ancestor.Path)
	must(t, err)
	legacyToken, err := ProjectToken(physical)
	must(t, err)
	mixed := *binding.Clone()
	mixed.WorkspaceConstraint.Token = legacyToken
	if selection, openErr := OpenPathProjectDirectory(t.Context(), mixed); openErr == nil {
		must(t, selection.Close())
		t.Fatal("path leaf accepted a legacy physical ancestor")
	}
	for _, relative := range []string{"../outside", "/absolute", "team/../project", "team\\project", ""} {
		changed := *binding.Clone()
		changed.WorkspaceConstraint.RelativeProject = relative
		if selection, openErr := OpenPathProjectDirectory(t.Context(), changed); openErr == nil {
			must(t, selection.Close())
			t.Fatalf("accepted relative selection %q", relative)
		}
	}
	must(t, os.Remove(leaf.Path))
	if selection, openErr := OpenPathProjectDirectory(t.Context(), binding); openErr == nil {
		must(t, selection.Close())
		t.Fatal("accepted missing project")
	}
	must(t, os.Symlink(t.TempDir(), leaf.Path))
	if selection, openErr := OpenPathProjectDirectory(t.Context(), binding); openErr == nil {
		must(t, selection.Close())
		t.Fatal("accepted escaping project symlink")
	}
	must(t, os.Remove(leaf.Path))
	must(t, os.Mkdir(leaf.Path, 0o700))
	alias := filepath.Join(ancestor.Path, "selected")
	must(t, os.Symlink(filepath.Join("team", "project"), alias))
	aliased := *binding.Clone()
	aliased.WorkspaceConstraint.RelativeProject = "selected"
	selection, err := OpenPathProjectDirectory(t.Context(), aliased)
	must(t, err)
	must(t, selection.Close())
	must(t, os.Mkdir(filepath.Join(ancestor.Path, "other"), 0o700))
	must(t, os.Remove(alias))
	must(t, os.Symlink("other", alias))
	if selection, openErr := OpenPathProjectDirectory(t.Context(), aliased); openErr == nil {
		must(t, selection.Close())
		t.Fatal("inside alias retargeting changed the frozen canonical leaf")
	}
	replacement := filepath.Join(t.TempDir(), "workspace")
	must(t, os.Rename(ancestor.Path, replacement))
	must(t, os.Symlink(replacement, ancestor.Path))
	if selection, openErr := OpenPathProjectDirectory(t.Context(), binding); openErr == nil {
		must(t, selection.Close())
		t.Fatal("accepted an ancestor whose approved path is now an alias")
	}
}

func TestPathProjectRequiresExplicitOfflineFeatureAndPreservesLegacy(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	path, err := ObservePathProjectContext(t.Context(), t.TempDir())
	must(t, err)
	token, err := ProjectToken(path)
	must(t, err)
	before := snapshotTree(t, store.Root)
	if _, err = store.RequireConnectorContext(t.Context(), root, token, path.Path); !errors.Is(err, ErrConnectorDisabled) {
		t.Fatalf("path request on old root=%v", err)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("path request silently upgraded the root")
	}
	must(t, store.EnableWorkspaceConnectorContext(t.Context(), true, true))
	features, err := store.ConnectorFeaturesContext(t.Context())
	must(t, err)
	if !slices.Contains(features, task.PathProjectConnectorFeature) {
		t.Fatal("offline cutover omitted path feature")
	}
	_, err = store.RequireConnectorContext(t.Context(), root, token, path.Path)
	must(t, err)
	physical, err := ObserveProjectContext(t.Context(), path.Path)
	must(t, err)
	legacy, err := ProjectToken(physical)
	must(t, err)
	replacePathDirectory(t, path.Path)
	if _, err = ResolveProjectTokenContext(t.Context(), legacy); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("new root weakened a v1 token: %v", err)
	}
	_, err = ResolveProjectTokenContext(t.Context(), token)
	must(t, err)
}

func TestPathProjectLeaseStillRefusesNativeRootReplacement(t *testing.T) {
	store := testStore(t)
	constraint, leaf := pathWorkspaceFixture(t, store)
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	token, err := ProjectToken(leaf)
	must(t, err)
	lease, err := store.RetainWorkspaceConnectorContext(t.Context(), root, token, constraint)
	must(t, err)
	t.Cleanup(func() { must(t, lease.Close()) })
	replacePathDirectory(t, store.Root)
	if err = lease.Validate(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("path approval relaxed native root custody: %v", err)
	}
}
