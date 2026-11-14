package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// workspaceSelection owns its open handles. Bindings retain approval authority;
// observations only check consistency of handles acquired by this operation.
type workspaceSelection struct {
	workspace        *os.Root
	workspaceFile    *os.File
	project          *os.Root
	projectFile      *os.File
	ancestor         task.DirectoryIdentity
	leaf             task.DirectoryIdentity
	constraint       task.WorkspaceConstraint
	observedAncestor task.DirectoryIdentity
	observedLeaf     task.DirectoryIdentity
}

func (w *workspaceSelection) Close() error {
	var failures []error
	if w.projectFile != nil {
		failures = append(failures, w.projectFile.Close())
	}
	if w.project != nil {
		failures = append(failures, w.project.Close())
	}
	if w.workspaceFile != nil {
		failures = append(failures, w.workspaceFile.Close())
	}
	if w.workspace != nil {
		failures = append(failures, w.workspace.Close())
	}
	return errors.Join(failures...)
}

func directoryIdentity(file *os.File, path string) (task.DirectoryIdentity, error) {
	held, err := heldDirectoryIdentity(file, path)
	if err != nil {
		return task.DirectoryIdentity{}, err
	}
	var visible unix.Stat_t
	if err := unix.Stat(path, &visible); err != nil {
		return task.DirectoryIdentity{}, err
	}
	if held.Device != uint64(visible.Dev) || held.Inode != uint64(visible.Ino) {
		return task.DirectoryIdentity{}, task.ErrIdentityMismatch
	}
	return held, nil
}

func heldDirectoryIdentity(file *os.File, path string) (task.DirectoryIdentity, error) {
	var held unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &held); err != nil {
		return task.DirectoryIdentity{}, err
	}
	if held.Mode&unix.S_IFMT != unix.S_IFDIR || held.Ino == 0 {
		return task.DirectoryIdentity{}, task.ErrIdentityMismatch
	}
	return task.DirectoryIdentity{Path: path, Device: uint64(held.Dev), Inode: uint64(held.Ino)}, nil
}

func openWorkspaceSelection(ctx context.Context, constraint *task.WorkspaceConstraint) (_ *workspaceSelection, resultErr error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if constraint == nil {
		return nil, task.ErrIdentityMismatch
	}
	if err := constraint.Validate(); err != nil {
		return nil, err
	}
	ancestor, err := parseProjectToken(constraint.Token)
	if err != nil {
		return nil, err
	}
	selection := &workspaceSelection{ancestor: ancestor, constraint: *constraint}
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, selection.Close())
		}
	}()
	if err = selection.open(ctx); err != nil {
		return nil, err
	}
	keep = true
	return selection, nil
}

func (w *workspaceSelection) open(ctx context.Context) error {
	var err error
	if w.ancestor.IsPath() {
		if err = requireCanonicalPath(w.ancestor.Path); err != nil {
			return err
		}
	}
	w.workspace, err = os.OpenRoot(w.ancestor.Path)
	if err != nil {
		return err
	}
	w.workspaceFile, err = w.workspace.Open(".")
	if err != nil {
		return err
	}
	actual, err := directoryIdentity(w.workspaceFile, w.ancestor.Path)
	if err != nil {
		return err
	}
	if !w.ancestor.MatchesObservation(actual) {
		return task.ErrIdentityMismatch
	}
	w.observedAncestor = actual
	w.project, err = w.workspace.OpenRoot(filepath.FromSlash(w.constraint.RelativeProject))
	if err != nil {
		return err
	}
	w.projectFile, err = w.project.Open(".")
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(filepath.Join(w.ancestor.Path, filepath.FromSlash(w.constraint.RelativeProject)))
	if err != nil {
		return err
	}
	inside, err := filepath.Rel(w.ancestor.Path, canonical)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return task.ErrIdentityMismatch
	}
	w.observedLeaf, err = directoryIdentity(w.projectFile, canonical)
	if err != nil {
		return err
	}
	w.leaf = w.observedLeaf
	if w.ancestor.IsPath() {
		w.leaf = task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: canonical}
	}
	return w.validateCurrentSelection(ctx)
}

func (w *workspaceSelection) Validate(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.ancestor.IsPath() {
		ancestor, err := heldDirectoryIdentity(w.workspaceFile, w.ancestor.Path)
		if err != nil {
			return err
		}
		leaf, err := heldDirectoryIdentity(w.projectFile, w.leaf.Path)
		if err != nil {
			return err
		}
		if ancestor != w.observedAncestor || leaf != w.observedLeaf {
			return task.ErrIdentityMismatch
		}
		return ctx.Err()
	}
	return w.validateCurrentSelection(ctx)
}

func (w *workspaceSelection) validateCurrentSelection(ctx context.Context) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.ancestor.IsPath() {
		if err := requireCanonicalPath(w.ancestor.Path); err != nil {
			return err
		}
	}
	ancestor, err := directoryIdentity(w.workspaceFile, w.ancestor.Path)
	if err != nil {
		return err
	}
	if ancestor != w.observedAncestor {
		return task.ErrIdentityMismatch
	}
	selected, err := w.workspace.Open(filepath.FromSlash(w.constraint.RelativeProject))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, selected.Close()) }()
	actual, err := directoryIdentity(selected, w.leaf.Path)
	if err != nil {
		return err
	}
	held, err := directoryIdentity(w.projectFile, w.leaf.Path)
	if err != nil {
		return err
	}
	if actual != w.observedLeaf || held != w.observedLeaf {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}

func requireCanonicalPath(path string) error {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if canonical != path {
		return task.ErrIdentityMismatch
	}
	return nil
}

// ProjectDirectorySelection owns the rooted handles for one current path
// selection. File is borrowed until Close and must not be retained or closed.
type ProjectDirectorySelection struct {
	selection *workspaceSelection
}

func (s *ProjectDirectorySelection) File() *os.File { return s.selection.projectFile }
func (s *ProjectDirectorySelection) Close() error   { return s.selection.Close() }

// OpenPathProjectDirectory resolves a saved path approval against the current
// filesystem. It reads no Store state and grants no task or root authority.
func OpenPathProjectDirectory(ctx context.Context, binding task.ConnectorContext) (*ProjectDirectorySelection, error) {
	if !binding.Project.IsPath() || binding.Project.Validate() != nil {
		return nil, task.ErrIdentityMismatch
	}
	if err := task.ValidateSHA256(binding.RootToken); err != nil {
		return nil, err
	}
	constraint := binding.WorkspaceConstraint
	if constraint == nil {
		token, err := ProjectToken(binding.Project)
		if err != nil {
			return nil, err
		}
		constraint = &task.WorkspaceConstraint{Token: token, RelativeProject: "."}
	}
	selection, err := openWorkspaceSelection(ctx, constraint)
	if err != nil {
		return nil, err
	}
	if selection.leaf != binding.Project {
		return nil, errors.Join(task.ErrIdentityMismatch, selection.Close())
	}
	return &ProjectDirectorySelection{selection: selection}, nil
}

func (s *Store) requirePathConnectorEnabled(ctx context.Context) error {
	if err := s.RequireWorkspaceConnectorEnabled(ctx); err != nil {
		return err
	}
	features, err := s.ConnectorFeaturesContext(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(features, task.PathProjectConnectorFeature) {
		return ErrConnectorDisabled
	}
	return nil
}

func (s *Store) RequireWorkspaceConnectorEnabled(ctx context.Context) error {
	if err := s.RequireConnectorEnabled(ctx); err != nil {
		return err
	}
	features, err := s.ConnectorFeaturesContext(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(features, task.WorkspaceConstrainedConnectorFeature) {
		return ErrConnectorDisabled
	}
	return nil
}

func (r *ReadOnlyStore) InspectWorkspaceConnectorContext(ctx context.Context, constraint *task.WorkspaceConstraint) (result ConnectorContextObservation, resultErr error) {
	result, err := r.InspectConnectorContext(ctx, "")
	if err != nil {
		return result, err
	}
	if err = r.store.RequireWorkspaceConnectorEnabled(ctx); err != nil {
		return result, err
	}
	selection, err := openWorkspaceSelection(ctx, constraint)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, selection.Close()) }()
	if selection.leaf.IsPath() {
		if err = r.store.requirePathConnectorEnabled(ctx); err != nil {
			return result, err
		}
	}
	result.ProjectToken, resultErr = ProjectToken(selection.leaf)
	result.WorkspaceConstraint = constraint.Clone()
	return result, resultErr
}

func (s *Store) RequireWorkspaceConnectorContext(ctx context.Context, root, project, cwd string, constraint *task.WorkspaceConstraint) (*task.ConnectorContext, error) {
	return s.requireConnectorContext(ctx, root, project, cwd, constraint)
}

func (r *ReadOnlyStore) RequireWorkspaceConnectorContext(ctx context.Context, root, project string, constraint *task.WorkspaceConstraint) (*task.ConnectorContext, error) {
	identity, err := parseProjectToken(project)
	if err != nil {
		return nil, err
	}
	return r.store.requireConnectorContext(ctx, root, project, identity.Path, constraint)
}

func (s *Store) RetainWorkspaceConnectorContext(ctx context.Context, root, project string, constraint *task.WorkspaceConstraint) (*ConnectorContextLease, error) {
	return s.retainConnectorContext(ctx, root, project, constraint)
}

func (s *Store) retainWorkspaceConnectorContext(ctx context.Context, root, project string, constraint *task.WorkspaceConstraint) (*ConnectorContextLease, error) {
	if err := s.RequireWorkspaceConnectorEnabled(ctx); err != nil {
		return nil, err
	}
	actualRoot, err := s.RootTokenContext(ctx)
	if err != nil {
		return nil, err
	}
	if actualRoot != root {
		return nil, task.ErrIdentityMismatch
	}
	selection, err := openWorkspaceSelection(ctx, constraint)
	if err != nil {
		return nil, err
	}
	token, err := ProjectToken(selection.leaf)
	if err != nil || token != project {
		return nil, errors.Join(task.ErrIdentityMismatch, err, selection.Close())
	}
	binding := task.ConnectorContext{RootToken: root, Project: selection.leaf, WorkspaceConstraint: constraint.Clone()}
	lease := &ConnectorContextLease{store: s, directory: selection.project, file: selection.projectFile, binding: binding, workspace: selection}
	if err = lease.Validate(ctx); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}

func (s *Store) retainPathConnectorContext(ctx context.Context, root string, project task.DirectoryIdentity, workspace *task.WorkspaceConstraint) (*ConnectorContextLease, error) {
	if err := s.requirePathConnectorEnabled(ctx); err != nil {
		return nil, err
	}
	actualRoot, err := s.RootTokenContext(ctx)
	if err != nil {
		return nil, err
	}
	if actualRoot != root {
		return nil, task.ErrIdentityMismatch
	}
	binding := task.ConnectorContext{RootToken: root, Project: project, WorkspaceConstraint: workspace.Clone()}
	selection, err := OpenPathProjectDirectory(ctx, binding)
	if err != nil {
		return nil, err
	}
	lease := &ConnectorContextLease{store: s, file: selection.File(), binding: binding, workspace: selection.selection}
	if err = lease.Validate(ctx); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}
