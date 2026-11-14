package taskdir

// Connector storage stays owner-local: it depends on Store's retained root,
// canonical staging, maintenance and task-owner exclusion. Exporting those
// authorities to a helper package would create a competing mutation boundary.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"golang.org/x/sys/unix"
)

var (
	ErrConnectorDisabled = errors.New("connector contract requires explicit offline root upgrade")
	ErrNamespaceRefused  = errors.New("task ID is permanently refused")
	ErrOriginClosed      = errors.New("origin admission generation is closed or superseded; creation not attempted")
)

// ConnectorContextObservation contains opaque identities, never filesystem paths.
type ConnectorContextObservation struct {
	ContractVersion     int                       `json:"contract_version"`
	Features            []string                  `json:"features"`
	RootToken           string                    `json:"root_token"`
	ProjectToken        string                    `json:"project_token,omitempty"`
	WorkspaceConstraint *task.WorkspaceConstraint `json:"workspace_constraint,omitempty"`
}

func identityToken(value any) (string, error) {
	data, err := task.MarshalCanonical(value)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func (s *Store) RootTokenContext(ctx context.Context) (string, error) {
	pin, err := s.PinRootContext(ctx)
	if err != nil {
		return "", err
	}
	return identityToken(struct {
		Version int    `json:"version"`
		RootID  string `json:"root_id"`
		Device  uint64 `json:"device"`
		Inode   uint64 `json:"inode"`
	}{1, pin.RootID(), pin.Device(), pin.Inode()})
}

func ObserveProjectContext(ctx context.Context, path string) (identity task.DirectoryIdentity, resultErr error) {
	if ctx == nil {
		return identity, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return identity, err
	}
	if !filepath.IsAbs(canonical) {
		return identity, task.ErrIdentityMismatch
	}
	directory, err := os.OpenRoot(canonical)
	if err != nil {
		return identity, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	file, err := directory.Open(".")
	if err != nil {
		return identity, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var held, visible unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &held); err != nil {
		return identity, err
	}
	if err = unix.Stat(canonical, &visible); err != nil {
		return identity, err
	}
	if held.Mode&unix.S_IFMT != unix.S_IFDIR || held.Ino == 0 || held.Dev != visible.Dev || held.Ino != visible.Ino {
		return identity, task.ErrIdentityMismatch
	}
	return task.DirectoryIdentity{Path: canonical, Device: uint64(held.Dev), Inode: uint64(held.Ino)}, ctx.Err()
}

// ObservePathProjectContext approves the current canonical pathname. Physical
// metadata is observed for acquisition consistency, never saved as authority.
func ObservePathProjectContext(ctx context.Context, path string) (task.DirectoryIdentity, error) {
	observed, err := ObserveProjectContext(ctx, path)
	if err != nil {
		return task.DirectoryIdentity{}, err
	}
	identity := task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: observed.Path}
	if err = identity.Validate(); err != nil {
		return task.DirectoryIdentity{}, err
	}
	return identity, nil
}

type projectContextToken struct {
	Version int                    `json:"version"`
	Project task.DirectoryIdentity `json:"project"`
}

type pathProjectContextToken struct {
	Version int                 `json:"version"`
	Project pathProjectIdentity `json:"project"`
}

type pathProjectIdentity struct {
	Path string `json:"path"`
}

func ProjectToken(identity task.DirectoryIdentity) (string, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	var value any = projectContextToken{1, identity}
	if identity.IsPath() {
		value = pathProjectContextToken{Version: 2, Project: pathProjectIdentity{Path: identity.Path}}
	}
	data, err := task.MarshalCanonical(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// ResolveProjectTokenContext validates the locally pinned opaque token through
// the current directory owner, including when no task request yet exists.
func ResolveProjectTokenContext(ctx context.Context, token string) (task.DirectoryIdentity, error) {
	project, err := parseProjectToken(token)
	if err != nil {
		return task.DirectoryIdentity{}, err
	}
	actual, err := ObserveProjectContext(ctx, project.Path)
	if err != nil {
		return task.DirectoryIdentity{}, err
	}
	if !project.MatchesObservation(actual) {
		return task.DirectoryIdentity{}, task.ErrIdentityMismatch
	}
	return project, nil
}

func parseProjectToken(token string) (task.DirectoryIdentity, error) {
	if len(token) > task.MaxControlRecordSize {
		return task.DirectoryIdentity{}, task.ErrControlRecordTooBig
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return task.DirectoryIdentity{}, task.ErrIdentityMismatch
	}
	var parsed struct {
		Version int             `json:"version"`
		Project json.RawMessage `json:"project"`
	}
	if err = task.DecodeStrict(data, &parsed); err != nil {
		return task.DirectoryIdentity{}, err
	}
	project, err := parseProjectTokenIdentity(parsed.Version, parsed.Project)
	if err != nil {
		return task.DirectoryIdentity{}, err
	}
	canonical, err := ProjectToken(project)
	if err != nil {
		return task.DirectoryIdentity{}, err
	}
	if canonical != token {
		return task.DirectoryIdentity{}, task.ErrIdentityMismatch
	}
	return project, nil
}

func parseProjectTokenIdentity(version int, data []byte) (task.DirectoryIdentity, error) {
	var project task.DirectoryIdentity
	switch version {
	case 1:
		if err := task.DecodeStrict(data, &project); err != nil {
			return task.DirectoryIdentity{}, err
		}
		if project.IsPath() {
			return task.DirectoryIdentity{}, task.ErrIdentityMismatch
		}
	case 2:
		var path pathProjectIdentity
		if err := task.DecodeStrict(data, &path); err != nil {
			return task.DirectoryIdentity{}, err
		}
		project = task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: path.Path}
	default:
		return task.DirectoryIdentity{}, task.ErrIdentityMismatch
	}
	if err := project.Validate(); err != nil {
		return task.DirectoryIdentity{}, err
	}
	return project, nil
}

// ConnectorControlGuard borrows the Store's physical root custody. It owns no
// project handle: saved task evidence and cancellation outlive the workspace.
// The caller must validate before acknowledgment, then close its owning Store.
type ConnectorControlGuard struct {
	store     *Store
	rootToken string
}

func (g *ConnectorControlGuard) Validate(ctx context.Context) error {
	if err := g.store.RequireConnectorEnabled(ctx); err != nil {
		return err
	}
	root, err := g.store.RootTokenContext(ctx)
	if err != nil {
		return err
	}
	if root != g.rootToken {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}

func (s *Store) BorrowConnectorControlContext(ctx context.Context, rootToken, projectToken string) (*ConnectorControlGuard, error) {
	if _, err := parseProjectToken(projectToken); err != nil {
		return nil, err
	}
	guard := &ConnectorControlGuard{store: s, rootToken: rootToken}
	if err := guard.Validate(ctx); err != nil {
		return nil, err
	}
	return guard, nil
}

func (r *ReadOnlyStore) BorrowConnectorControlContext(ctx context.Context, rootToken, projectToken string) (*ConnectorControlGuard, error) {
	return r.store.BorrowConnectorControlContext(ctx, rootToken, projectToken)
}

func (r *ReadOnlyStore) RequireConnectorContext(ctx context.Context, expectedRoot, expectedProject string) (*task.ConnectorContext, error) {
	project, err := ResolveProjectTokenContext(ctx, expectedProject)
	if err != nil {
		return nil, err
	}
	return r.store.RequireConnectorContext(ctx, expectedRoot, expectedProject, project.Path)
}

func (s *Store) RequireConnectorContext(ctx context.Context, expectedRoot, expectedProject, cwd string) (*task.ConnectorContext, error) {
	return s.requireConnectorContext(ctx, expectedRoot, expectedProject, cwd, nil)
}

func (s *Store) requireConnectorContext(ctx context.Context, expectedRoot, expectedProject, cwd string, workspace *task.WorkspaceConstraint) (_ *task.ConnectorContext, resultErr error) {
	if err := s.RequireConnectorEnabled(ctx); err != nil {
		return nil, err
	}
	project, err := parseProjectToken(expectedProject)
	if err != nil {
		return nil, err
	}
	if project.IsPath() {
		if err = s.requirePathConnectorEnabled(ctx); err != nil {
			return nil, err
		}
	}
	root, err := s.RootTokenContext(ctx)
	if err != nil {
		return nil, err
	}
	actual, err := ObserveProjectContext(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if !project.MatchesObservation(actual) {
		return nil, task.ErrIdentityMismatch
	}
	if workspace != nil {
		if err = s.RequireWorkspaceConnectorEnabled(ctx); err != nil {
			return nil, err
		}
		selection, selectErr := openWorkspaceSelection(ctx, workspace)
		if selectErr != nil {
			return nil, selectErr
		}
		defer func() { resultErr = errors.Join(resultErr, selection.Close()) }()
		if project != selection.leaf {
			return nil, task.ErrIdentityMismatch
		}
	}
	if root != expectedRoot {
		return nil, task.ErrIdentityMismatch
	}
	return &task.ConnectorContext{RootToken: root, Project: project, WorkspaceConstraint: workspace.Clone()}, nil
}

func (s *Store) RequireConnectorEnabled(ctx context.Context) error {
	record, err := readStoreRootRecord(ctx, s)
	if err != nil {
		return err
	}
	if !slices.Contains(record.RequiredFeatures, task.NamespaceAdmissionFeature) || !slices.Contains(record.RequiredFeatures, task.NamespaceMutationFeature) {
		return ErrConnectorDisabled
	}
	return s.verifyNamespaceMutationLock(ctx)
}

// ConnectorFeaturesContext returns the actual root's immutable feature projection.
func (s *Store) ConnectorFeaturesContext(ctx context.Context) ([]string, error) {
	record, err := readStoreRootRecord(ctx, s)
	if err != nil {
		return nil, err
	}
	if slices.Contains(record.RequiredFeatures, task.NamespaceMutationFeature) {
		if err = s.verifyNamespaceMutationLock(ctx); err != nil {
			return nil, err
		}
	}
	return append([]string{}, record.RequiredFeatures...), nil
}

func (r *ReadOnlyStore) ConnectorFeaturesContext(ctx context.Context) ([]string, error) {
	return r.store.ConnectorFeaturesContext(ctx)
}

func (r *ReadOnlyStore) InspectConnectorContext(ctx context.Context, cwd string) (ConnectorContextObservation, error) {
	result := ConnectorContextObservation{ContractVersion: 1, Features: []string{}}
	features, err := r.store.ConnectorFeaturesContext(ctx)
	if err != nil {
		return result, err
	}
	result.Features = features
	result.RootToken, err = r.store.RootTokenContext(ctx)
	if err != nil || cwd == "" {
		return result, err
	}
	project, err := ObserveProjectContext(ctx, cwd)
	if err != nil {
		return result, err
	}
	result.ProjectToken, err = ProjectToken(project)
	return result, err
}

// EnableConnectorContext is a one-way offline operator transition. The boolean
// attests external process/service closure; this method cannot prove that closure.
func (s *Store) EnableConnectorContext(ctx context.Context, offlineCutoverConfirmed bool) error {
	return s.EnableWorkspaceConnectorContext(ctx, offlineCutoverConfirmed, false)
}

func (s *Store) EnableWorkspaceConnectorContext(ctx context.Context, offlineCutoverConfirmed, workspace bool) (resultErr error) {
	if !offlineCutoverConfirmed {
		return errors.New("offline cutover attestation required: stop/drain old writers and frontends first")
	}
	if err := s.maintLock.LockEXNonblockingContext(ctx); err != nil {
		return err
	}
	defer s.finishConnectorMaintenance(&resultErr)
	if _, err := s.PinRootContext(ctx); err != nil {
		return err
	}
	record, err := readStoreRootRecord(ctx, s)
	if err != nil {
		return err
	}
	changed, err := s.prepareConnectorFeaturesContext(ctx, record)
	if err != nil {
		return err
	}
	if addWorkspaceConnectorFeatures(record, workspace) {
		changed = true
	}
	if !changed {
		return s.barrierDir(s.Root)
	}
	data, err := marshalControlRecord(record)
	if err != nil {
		return err
	}
	id, err := randomid.NewRandomID()
	if err != nil {
		return err
	}
	name := "root-upgrade." + id + ".json"
	_, cleanupErr, writeErr := s.stageReaderOwnedContext(ctx, s.Root, name, bytes.NewReader(data), s.faultInjector)
	if err = errors.Join(writeErr, cleanupErr); err != nil {
		return err
	}
	parent, err := s.openDir(s.Root)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	if err = unix.Renameat(int(parent.Fd()), name, int(parent.Fd()), "root.json"); err != nil {
		return err
	}
	if err = s.barrierDir(s.Root); err != nil {
		return fmt.Errorf("%w: root upgrade: %w", task.ErrUncertainDurability, err)
	}
	return s.RequireConnectorEnabled(ctx)
}

func addWorkspaceConnectorFeatures(record *task.RootRecord, workspace bool) bool {
	if !workspace {
		return false
	}
	changed := false
	for _, feature := range []string{task.WorkspaceConstrainedConnectorFeature, task.PathProjectConnectorFeature} {
		if !slices.Contains(record.RequiredFeatures, feature) {
			record.RequiredFeatures = append(record.RequiredFeatures, feature)
			changed = true
		}
	}
	return changed
}

func (s *Store) validateConnectorRecordContext(ctx context.Context, req *task.TaskRecord) error {
	if req.ConnectorContext == nil {
		return nil
	}
	expected, err := ProjectToken(req.ConnectorContext.Project)
	if err != nil {
		return err
	}
	_, err = s.requireConnectorContext(ctx, req.ConnectorContext.RootToken, expected, req.CanonicalCwd, req.ConnectorContext.WorkspaceConstraint)
	return err
}

// ConnectorContextLease retains the project directory and store root through
// a guarded operation. Validation is repeated immediately before acknowledgment.
// The caller owns Close before closing its Store/ReadOnlyStore.
type ConnectorContextLease struct {
	store     *Store
	directory *os.Root
	file      *os.File
	binding   task.ConnectorContext
	workspace *workspaceSelection
}

func (l *ConnectorContextLease) Binding() task.ConnectorContext { return *l.binding.Clone() }
func (l *ConnectorContextLease) Validate(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.binding.Project.IsPath() {
		return l.validatePath(ctx)
	}
	return l.validatePhysical(ctx)
}

func (l *ConnectorContextLease) validatePhysical(ctx context.Context) error {
	if l.workspace != nil {
		if err := l.store.RequireWorkspaceConnectorEnabled(ctx); err != nil {
			return err
		}
		if err := l.workspace.Validate(ctx); err != nil {
			return err
		}
	}
	root, err := l.store.RootTokenContext(ctx)
	if err != nil {
		return err
	}
	if root != l.binding.RootToken {
		return task.ErrIdentityMismatch
	}
	canonical, err := filepath.EvalSymlinks(l.binding.Project.Path)
	if err != nil {
		return err
	}
	if canonical != l.binding.Project.Path {
		return task.ErrIdentityMismatch
	}
	var held, visible unix.Stat_t
	if err = unix.Fstat(int(l.file.Fd()), &held); err != nil {
		return err
	}
	if err = unix.Stat(canonical, &visible); err != nil {
		return err
	}
	if uint64(held.Dev) != l.binding.Project.Device || uint64(held.Ino) != l.binding.Project.Inode || held.Dev != visible.Dev || held.Ino != visible.Ino {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}

func (l *ConnectorContextLease) validatePath(ctx context.Context) error {
	if err := l.store.requirePathConnectorEnabled(ctx); err != nil {
		return err
	}
	root, err := l.store.RootTokenContext(ctx)
	if err != nil {
		return err
	}
	if root != l.binding.RootToken || l.workspace == nil {
		return task.ErrIdentityMismatch
	}
	return l.workspace.Validate(ctx)
}

func (l *ConnectorContextLease) Close() error {
	if l.workspace != nil {
		return l.workspace.Close()
	}
	return errors.Join(l.file.Close(), l.directory.Close())
}

func (s *Store) RetainConnectorContext(ctx context.Context, rootToken, projectToken string) (*ConnectorContextLease, error) {
	return s.retainConnectorContext(ctx, rootToken, projectToken, nil)
}

func (s *Store) retainConnectorContext(ctx context.Context, rootToken, projectToken string, workspace *task.WorkspaceConstraint) (*ConnectorContextLease, error) {
	identity, err := parseProjectToken(projectToken)
	if err != nil {
		return nil, err
	}
	if identity.IsPath() {
		return s.retainPathConnectorContext(ctx, rootToken, identity, workspace)
	}
	if workspace != nil {
		return s.retainWorkspaceConnectorContext(ctx, rootToken, projectToken, workspace)
	}
	project, err := ResolveProjectTokenContext(ctx, projectToken)
	if err != nil {
		return nil, err
	}
	binding, err := s.RequireConnectorContext(ctx, rootToken, projectToken, project.Path)
	if err != nil {
		return nil, err
	}
	directory, err := os.OpenRoot(project.Path)
	if err != nil {
		return nil, err
	}
	file, err := directory.Open(".")
	if err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	lease := &ConnectorContextLease{store: s, directory: directory, file: file, binding: *binding}
	if err = lease.Validate(ctx); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}

func (r *ReadOnlyStore) RetainConnectorContext(ctx context.Context, rootToken, projectToken string) (*ConnectorContextLease, error) {
	return r.store.RetainConnectorContext(ctx, rootToken, projectToken)
}

// NamespaceRefusal permanently consumes an ordinary TaskID without a task outcome.
// Fragments remain evidence. Unattributed refusals require explicit local selection.
type NamespaceRefusal struct {
	SchemaVersion int                 `json:"schema_version"`
	RootID        string              `json:"root_id"`
	TaskID        string              `json:"task_id"`
	Origin        *task.GatewayOrigin `json:"origin,omitempty"`
	Unattributed  bool                `json:"unattributed"`
}

type NamespaceResolution struct {
	Resolution string            `json:"resolution"`
	Refusal    *NamespaceRefusal `json:"refusal,omitempty"`
}

func (s *Store) taskNamespaceContext(ctx context.Context, id string) (*LockFile, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	directory := filepath.Join(s.Root, "task-namespace")
	if err := s.mkdirContext(ctx, directory, s.faultInjector); err != nil {
		return nil, err
	}
	lock, err := s.openLock(filepath.Join(directory, id+".lock"), LockLevelNone)
	if err != nil {
		return nil, err
	}
	if err = lock.LockEXNonblockingContext(ctx); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return lock, nil
}

// NamespaceCleanupError retains namespace and its operation ancestor leases
// after an unlock fault. The caller may retry Release; it grants no new action.
type NamespaceCleanupError struct {
	store                *Store
	lock                 *LockFile
	cause                error
	namespaceReleased    bool
	namespaceClosed      bool
	ancestors            *connectorAncestors
	ancestorsTransferred bool
}

func (e *NamespaceCleanupError) Error() string         { return e.cause.Error() }
func (e *NamespaceCleanupError) Unwrap() error         { return e.cause }
func (e *NamespaceCleanupError) RetainedStore() *Store { return e.store }
func (e *NamespaceCleanupError) Release() error {
	if e.store == nil {
		return nil
	}
	if !e.namespaceReleased {
		if err := e.lock.Unlock(); err != nil {
			return err
		}
		e.namespaceReleased = true
	}
	if !e.namespaceClosed {
		if err := e.lock.closeIdle(); err != nil {
			return err
		}
		e.namespaceClosed = true
	}
	if !e.ancestorsTransferred {
		if err := e.ancestors.release(); err != nil {
			return err
		}
	}
	e.store = nil
	return nil
}

func (s *Store) closeNamespace(lock *LockFile) error {
	if lock == nil {
		return nil
	}
	if err := lock.Unlock(); err != nil {
		return &NamespaceCleanupError{store: s, lock: lock, ancestors: &connectorAncestors{store: s}, cause: err}
	}
	return lock.closeIdle()
}

func (s *Store) ReadNamespaceRefusalContext(ctx context.Context, id string) (*NamespaceRefusal, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	var refusal NamespaceRefusal
	err := s.readRecordContext(ctx, filepath.Join(s.Root, "task-refusals", id+".json"), &refusal)
	if task.RecordAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if refusal.SchemaVersion != task.SchemaVersion || refusal.RootID != s.RootID || refusal.TaskID != id {
		return nil, task.ErrIdentityMismatch
	}
	if refusal.Origin != nil {
		if err = task.ValidateGatewayOrigin(*refusal.Origin); err != nil {
			return nil, err
		}
	}
	if refusal.Unattributed && refusal.Origin != nil {
		return nil, task.ErrEvidenceFault
	}
	if err = s.validateRefusedTaskFragmentsContext(ctx, id); err != nil {
		return nil, err
	}
	return &refusal, nil
}

func (r *ReadOnlyStore) ReadNamespaceRefusalContext(ctx context.Context, id string) (*NamespaceRefusal, error) {
	return r.store.ReadNamespaceRefusalContext(ctx, id)
}

func (s *Store) requireNamespaceUnrefusedContext(ctx context.Context, id string) error {
	refusal, err := s.ReadNamespaceRefusalContext(ctx, id)
	if err != nil {
		return err
	}
	if refusal != nil {
		return ErrNamespaceRefused
	}
	return nil
}

// FenceTaskNamespaceContext atomically fences absence. Existing entries are
// never touched here: partial proof uses its separate ordered owner path.
func (s *Store) FenceTaskNamespaceContext(ctx context.Context, id string, origin *task.GatewayOrigin) (result NamespaceResolution, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = NamespaceResolution{Resolution: "unresolved"}
		}
	}()
	if err := s.RequireConnectorEnabled(ctx); err != nil {
		return result, err
	}
	if origin != nil {
		if err := task.ValidateGatewayOrigin(*origin); err != nil {
			return result, err
		}
	}
	ancestors, err := s.beginConnectorMutationContext(ctx, false)
	if err != nil {
		return result, err
	}
	defer s.finishConnectorAncestors(&resultErr, ancestors)
	namespace, err := s.taskNamespaceContext(ctx, id)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, s.closeNamespace(namespace)) }()
	if _, err = s.PinRootContext(ctx); err != nil {
		return result, err
	}
	refusal, err := s.ReadNamespaceRefusalContext(ctx, id)
	if err != nil {
		return result, err
	}
	if refusal != nil {
		verified, verifyErr := s.verifyNamespaceRefusalOwnedContext(ctx, id, refusal)
		if verifyErr != nil {
			return result, verifyErr
		}
		return namespaceRefusalResolution(verified, origin)
	}
	exists, err := s.TaskEntryExists(id)
	if err != nil {
		return result, err
	}
	if exists {
		return NamespaceResolution{Resolution: "existing"}, nil
	}
	candidate := NamespaceRefusal{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: id, Origin: origin}
	return s.commitNamespaceRefusalContext(ctx, candidate)
}

func namespaceRefusalResolution(refusal *NamespaceRefusal, origin *task.GatewayOrigin) (NamespaceResolution, error) {
	if origin != nil && refusal.Origin != nil && !reflect.DeepEqual(origin, refusal.Origin) {
		return NamespaceResolution{}, task.ErrRequestConflict
	}
	return NamespaceResolution{Resolution: "permanently_refused", Refusal: refusal}, nil
}

func (s *Store) commitNamespaceRefusalContext(ctx context.Context, candidate NamespaceRefusal) (NamespaceResolution, error) {
	directory := filepath.Join(s.Root, "task-refusals")
	if err := s.mkdirContext(ctx, directory, s.faultInjector); err != nil {
		return NamespaceResolution{}, err
	}
	data, err := marshalControlRecord(candidate)
	if err != nil {
		return NamespaceResolution{}, err
	}
	if err = s.commitConnectorRecordContext(ctx, directory, candidate.TaskID+".json", data); err != nil {
		return NamespaceResolution{}, err
	}
	winner, err := s.verifyNamespaceRefusalOwnedContext(ctx, candidate.TaskID, &candidate)
	if err != nil {
		return NamespaceResolution{}, err
	}
	return NamespaceResolution{Resolution: "permanently_refused", Refusal: winner}, nil
}

// PartialFenceSelection names the immutable evidence or explicit operator
// authority for one partial-recovery attempt. A supplied digest always guards
// the actual task.json bytes, independently of attribution.
type PartialFenceSelection struct {
	Frozen               *RecordedTaskIdentity
	OperatorUnattributed bool
	ExpectedSpecSHA256   string
}

// FencePartialTaskContext is explicit recovery only. A nil frozen request is
// permitted solely with local operator authorization, never a generic scan.
// Lock order: maintenance -> mutation -> admission -> lifetime -> run -> nonblocking namespace.
func (s *Store) FencePartialTaskContext(ctx context.Context, id string, selection PartialFenceSelection) (result NamespaceResolution, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = NamespaceResolution{Resolution: "unresolved"}
		}
	}()
	if err := s.validatePartialFenceRequest(id, selection); err != nil {
		return result, err
	}
	if err := s.RequireConnectorEnabled(ctx); err != nil {
		return result, err
	}
	ancestors, err := s.beginConnectorMutationContext(ctx, false)
	if err != nil {
		return result, err
	}
	defer s.finishConnectorAncestors(&resultErr, ancestors)
	exists, err := s.TaskEntryExists(id)
	if err != nil {
		return result, err
	}
	if !exists {
		if selection.ExpectedSpecSHA256 != "" {
			return result, fmt.Errorf("%w: expected spec requires recorded task bytes", task.ErrEvidenceFault)
		}
		return NamespaceResolution{Resolution: "unresolved"}, nil
	}
	td, err := s.newTaskHandle(id, true)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	return td.withPartialFenceExclusionContext(ctx, func() (NamespaceResolution, error) {
		return td.fencePartialPreparationContext(ctx, selection)
	})
}

// fencePartialPreparationContext runs only under maintenance, admission,
// lifetime and run exclusion. It owns the final namespace proof and commit.
func (td *TaskDir) fencePartialPreparationContext(ctx context.Context, selection PartialFenceSelection) (result NamespaceResolution, resultErr error) {
	namespace, err := td.store.taskNamespaceContext(ctx, td.TaskID)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.closeNamespace(namespace)) }()
	if _, err = td.store.PinRootContext(ctx); err != nil {
		return result, err
	}
	current, err := td.recordedTaskIdentityContext(ctx)
	if err != nil && !task.RecordAbsent(err) {
		return result, err
	}
	if err = selection.matchIdentity(current); err != nil {
		return result, err
	}
	frozen := selection.Frozen
	completeErr := td.preparedIdentityCompleteContext(ctx, current)
	if completeErr == nil {
		return NamespaceResolution{Resolution: "existing"}, nil
	}
	if !task.RecordAbsent(completeErr) {
		return result, completeErr
	}
	if err = td.provePreparationOnlyContext(ctx, current); err != nil {
		return NamespaceResolution{Resolution: "unresolved"}, err
	}
	// Recorded identities passed ValidateConnectorBinding, which requires origin
	// and connector context together. Origin presence is the attribution check.
	if frozen == nil && current != nil && current.request.Origin != nil {
		return NamespaceResolution{Resolution: "existing"}, nil
	}
	candidate := NamespaceRefusal{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, Unattributed: frozen == nil}
	if frozen != nil {
		candidate.Origin = frozen.request.Origin.Clone()
	}
	return td.store.commitNamespaceRefusalContext(ctx, candidate)
}

// matchIdentity applies every supplied guard to the same recorded bytes.
func (selection PartialFenceSelection) matchIdentity(current *RecordedTaskIdentity) error {
	if selection.ExpectedSpecSHA256 != "" {
		if current == nil {
			return fmt.Errorf("%w: expected spec requires recorded task bytes", task.ErrEvidenceFault)
		}
		if task.ComputeSHA256(current.data) != selection.ExpectedSpecSHA256 {
			return task.ErrRequestConflict
		}
	}
	if selection.Frozen != nil && (current == nil || !bytes.Equal(current.data, selection.Frozen.data)) {
		return task.ErrRequestConflict
	}
	return nil
}

func (s *Store) validatePartialFenceRequest(id string, selection PartialFenceSelection) error {
	if err := task.ValidateTaskID(id); err != nil {
		return err
	}
	if selection.ExpectedSpecSHA256 != "" {
		if err := task.ValidateSHA256(selection.ExpectedSpecSHA256); err != nil {
			return err
		}
	}
	frozen := selection.Frozen
	if (frozen == nil && !selection.OperatorUnattributed) || (frozen != nil && selection.OperatorUnattributed) {
		return errors.New("partial refusal requires exactly one frozen request or explicit local operator selection")
	}
	if frozen == nil {
		return nil
	}
	if err := task.ValidateTaskRecord(&frozen.request); err != nil {
		return err
	}
	if frozen.request.RootID != s.RootID || frozen.request.TaskID != id || len(frozen.data) == 0 {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (td *TaskDir) withPartialFenceExclusionContext(ctx context.Context, operation func() (NamespaceResolution, error)) (result NamespaceResolution, resultErr error) {
	resultErr = td.withConnectorTaskExclusionContext(ctx, true, func() error {
		var err error
		result, err = operation()
		return err
	})
	return result, resultErr
}

// ConnectorTaskCleanupError retains the exact task exclusions and their
// operation ancestor leases after cleanup fails. Release grants no work.
type ConnectorTaskCleanupError struct {
	store                     *Store
	task                      *TaskDir
	owner                     *LifetimeOwnerLease
	admissionHeld, runHeld    bool
	ancestors                 *connectorAncestors
	creationNamespace         *LockFile
	creationNamespaceReleased bool
	cause                     error
}

func (e *ConnectorTaskCleanupError) Error() string         { return e.cause.Error() }
func (e *ConnectorTaskCleanupError) Unwrap() error         { return e.cause }
func (e *ConnectorTaskCleanupError) RetainedStore() *Store { return e.store }
func (e *ConnectorTaskCleanupError) releaseTaskExclusions() error {
	if CleanupRetainsStore(e.cause, e.store) {
		return e.cause
	}
	if e.runHeld {
		if err := e.task.runLock.Unlock(); err != nil {
			return err
		}
		e.runHeld = false
	}
	if e.owner != nil {
		if err := e.owner.Release(); err != nil {
			return err
		}
		e.owner = nil
	}
	if e.admissionHeld {
		if err := e.task.admissionLock.Unlock(); err != nil {
			return err
		}
		e.admissionHeld = false
	}
	return nil
}

func (e *ConnectorTaskCleanupError) Release() error {
	if e.store == nil {
		return nil
	}
	if err := e.releaseTaskExclusions(); err != nil {
		return err
	}
	if err := e.task.Close(); err != nil {
		return err
	}
	if e.creationNamespace != nil {
		if !e.creationNamespaceReleased {
			if err := e.creationNamespace.Unlock(); err != nil {
				return err
			}
			e.creationNamespaceReleased = true
		}
		if err := e.creationNamespace.closeIdle(); err != nil {
			return err
		}
		e.creationNamespace = nil
	}
	if err := e.ancestors.release(); err != nil {
		return err
	}
	e.store = nil
	return nil
}

func (td *TaskDir) withConnectorTaskExclusionContext(ctx context.Context, createOwner bool, operation func() error) (resultErr error) {
	return td.withTaskObservationExclusionContext(ctx, createOwner, false, operation)
}

func (td *TaskDir) withTaskObservationExclusionContext(ctx context.Context, createOwner, preparationOnly bool, operation func() error) (resultErr error) {
	cleanup := &ConnectorTaskCleanupError{store: td.store, task: td, ancestors: &connectorAncestors{store: td.store}}
	defer func() {
		cleanup.cause = resultErr
		// Transfer the ancestor lease from a nested namespace cleanup to this
		// wider cleanup owner. Namespace Release must settle first, while this
		// owner keeps maintenance held through lifetime and admission release.
		task.AnyErrorNode(resultErr, func(node error) bool {
			var namespace *NamespaceCleanupError
			if errors.As(node, &namespace) && namespace.RetainedStore() == td.store {
				namespace.ancestorsTransferred = true
			}
			return false
		})
		if err := cleanup.releaseTaskExclusions(); err != nil {
			cleanup.cause = errors.Join(resultErr, err)
			resultErr = cleanup
		}
	}()
	if err := td.admissionLock.LockEXNonblockingContext(ctx); err != nil {
		if !createOwner {
			return connectorClosureObservationError(err, ConnectorClosureMissingOwner)
		}
		return err
	}
	cleanup.admissionHeld = true
	var err error
	cleanup.owner, err = td.tryAcquireLifetimeOwner(ctx, createOwner)
	if preparationOnly && task.RecordAbsent(err) {
		err = nil
	}
	if err != nil {
		if !createOwner {
			return connectorClosureObservationError(err, ConnectorClosureMissingOwner)
		}
		return err
	}
	if err = td.runLock.LockEXNonblockingContext(ctx); err != nil {
		if !createOwner {
			return connectorClosureObservationError(err, ConnectorClosureMissingOwner)
		}
		return err
	}
	cleanup.runHeld = true
	return operation()
}

// Exact enumeration excludes ALL execution/control evidence, including orphaned
// receipts and native/terminal records; absence of two selected files is not proof.
func (td *TaskDir) provePreparationOnlyContext(ctx context.Context, frozen *RecordedTaskIdentity) error {
	directory, err := td.store.openDir(td.Dir)
	if err != nil {
		return err
	}
	return scanDirectoryNames(func() error { return ctx.Err() }, directory, func(name string) error {
		switch name {
		case ".admission.lock", ".run.lock":
			_, readErr := td.store.readBytesContext(ctx, filepath.Join(td.Dir, name), 0)
			return readErr
		case "lifetime":
			life, openErr := td.store.openDir(filepath.Join(td.Dir, name))
			if openErr != nil {
				return openErr
			}
			return scanDirectoryNames(func() error { return ctx.Err() }, life, func(child string) error {
				if child != lifetimeOwnerLockName {
					return task.ErrEvidenceFault
				}
				_, readErr := td.store.readBytesContext(ctx, filepath.Join(td.Dir, name, child), 0)
				return readErr
			})
		case "brief.md", "task.json", "meta.json":
			return td.validatePreparationFragmentContext(ctx, name, frozen)
		default:
			if isStageFile(name) && frozen != nil {
				return td.validatePreparationStageContext(ctx, name, frozen)
			}
			return fmt.Errorf("%w: partial task has unclassified preparation or execution evidence", task.ErrEvidenceFault)
		}
	})
}

func (td *TaskDir) validatePreparationFragmentContext(ctx context.Context, name string, frozen *RecordedTaskIdentity) error {
	if name == "task.json" {
		// This exact request buffer was read and decoded under the same owner
		// exclusions. Neither the spec check nor this proof reads a second copy.
		if frozen == nil {
			return task.ErrEvidenceFault
		}
		return ctx.Err()
	}
	limit := int64(task.MaxControlRecordSize)
	if name == "brief.md" {
		limit = task.MaxBriefSize
	}
	data, err := td.store.readBytesContext(ctx, filepath.Join(td.Dir, name), limit)
	if err != nil {
		return err
	}
	switch name {
	case "brief.md":
		if frozen != nil && (int64(len(data)) != frozen.request.BriefLength || task.ComputeSHA256(data) != frozen.request.BriefSHA256) {
			return task.ErrRequestConflict
		}
	case "meta.json":
		return td.validatePreparationMeta(data, frozen)
	}
	return nil
}

func (td *TaskDir) validatePreparationStageContext(ctx context.Context, name string, frozen *RecordedTaskIdentity) error {
	data, err := td.store.readBytesContext(ctx, filepath.Join(td.Dir, name), task.MaxBriefSize)
	if err != nil {
		return err
	}
	if int64(len(data)) == frozen.request.BriefLength && task.ComputeSHA256(data) == frozen.request.BriefSHA256 {
		return nil
	}
	if bytes.Equal(data, frozen.data) {
		return nil
	}
	return td.validatePreparationMeta(data, frozen)
}

// OriginAdmission is root-owned generation state, never a task execution ledger.
type OriginAdmission struct {
	SchemaVersion      int    `json:"schema_version"`
	RootID             string `json:"root_id"`
	GatewayID          string `json:"gateway_id"`
	RecoveryID         string `json:"recovery_id"`
	ExpectedGeneration string `json:"expected_generation"`
	Generation         string `json:"generation"`
	AdmissionOpen      bool   `json:"admission_open"`
	Sequence           uint64 `json:"sequence"`
	PreviousSHA256     string `json:"previous_sha256,omitempty"`
	MembershipSHA256   string `json:"membership_sha256,omitempty"`
}

type OriginInventoryEntry struct {
	Receipt     *ConnectorReceipt   `json:"receipt,omitempty"`
	TaskID      string              `json:"task_id"`
	Resolution  string              `json:"resolution"`
	Origin      *task.GatewayOrigin `json:"origin,omitempty"`
	SpecSHA256  string              `json:"spec_sha256,omitempty"`
	BriefSHA256 string              `json:"brief_sha256,omitempty"`
}

type OriginInventory struct {
	Entries             []OriginInventoryEntry `json:"entries"`
	NextCursor          string                 `json:"next_cursor,omitempty"`
	Complete            bool                   `json:"complete"`
	MembershipSHA256    string                 `json:"membership_sha256"`
	CompletenessReceipt string                 `json:"completeness_receipt,omitempty"`
}

func (s *Store) originDirectory(gateway string) string {
	return filepath.Join(s.Root, "gateway-origins", task.ComputeSHA256([]byte(gateway)))
}

func (s *Store) readOriginAdmissionContext(ctx context.Context, gateway string) (*OriginAdmission, error) {
	if err := task.ValidateConnectorID(gateway); err != nil {
		return nil, err
	}
	directory, err := s.openDir(s.originDirectory(gateway))
	if directoryAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names, readErr := directory.Readdirnames(-1)
	if err = errors.Join(readErr, directory.Close(), ctx.Err()); err != nil {
		return nil, err
	}
	slices.Sort(names)
	var previous *OriginAdmission
	var digest string
	for _, name := range names {
		if isStageFile(name) {
			continue
		}
		record, current, readErr := s.readOriginRecordContext(ctx, gateway, name)
		if readErr != nil {
			return nil, readErr
		}
		if err = record.validateSuccessor(previous, digest); err != nil {
			return nil, err
		}
		previous, digest = record, current
	}
	return previous, nil
}

func (s *Store) readOriginRecordContext(ctx context.Context, gateway, name string) (*OriginAdmission, string, error) {
	sequence, err := strconv.ParseUint(strings.TrimSuffix(name, ".json"), 10, 64)
	if err != nil || name != fmt.Sprintf("%020d.json", sequence) {
		return nil, "", task.ErrEvidenceFault
	}
	data, err := s.readBytesContext(ctx, filepath.Join(s.originDirectory(gateway), name), task.MaxControlRecordSize)
	if err != nil {
		return nil, "", err
	}
	var record OriginAdmission
	if err = task.DecodeStrict(data, &record); err != nil {
		return nil, "", err
	}
	if record.SchemaVersion != task.SchemaVersion || record.RootID != s.RootID || record.GatewayID != gateway || record.Sequence != sequence {
		return nil, "", task.ErrIdentityMismatch
	}
	for _, id := range []string{record.RecoveryID, record.Generation} {
		if err = task.ValidateConnectorID(id); err != nil {
			return nil, "", err
		}
	}
	return &record, task.ComputeSHA256(data), nil
}

func (r OriginAdmission) validateSuccessor(previous *OriginAdmission, digest string) error {
	expected := uint64(1)
	if previous != nil {
		expected = previous.Sequence + 1
	}
	if r.Sequence != expected || r.PreviousSHA256 != digest {
		return task.ErrIdentityMismatch
	}
	if !r.AdmissionOpen {
		return nil
	}
	if previous == nil || previous.AdmissionOpen || previous.Generation != r.Generation || previous.RecoveryID != r.RecoveryID {
		return task.ErrInvariantFault
	}
	return nil
}

func isStageFile(name string) bool {
	return len(name) == 42 && strings.HasPrefix(name, "stage.") && strings.HasSuffix(name, ".tmp") && task.ValidateTaskID(name[6:38]) == nil
}

func (s *Store) requireOriginAdmissionContext(ctx context.Context, origin *task.GatewayOrigin) error {
	if origin == nil {
		return nil
	}
	state, err := s.readOriginAdmissionContext(ctx, origin.GatewayID)
	if err != nil {
		return err
	}
	if state == nil || !state.AdmissionOpen || state.Generation != origin.AdmissionGeneration {
		return ErrOriginClosed
	}
	return nil
}

// OriginBarrierContext excludes membership mutations without blocking live tasks.
// Contention is pending; callers retry the SAME explicit recovery transition.
func (s *Store) OriginBarrierContext(ctx context.Context, gateway, recovery, expected, generation, rootToken string) (result *OriginAdmission, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	if err := validateOriginBarrierInput(gateway, recovery, expected, generation); err != nil {
		return nil, err
	}
	ancestors, err := s.beginConnectorMutationContext(ctx, true)
	if err != nil {
		return nil, err
	}
	defer s.finishConnectorAncestors(&resultErr, ancestors)
	if err = s.requireOriginRootContext(ctx, rootToken); err != nil {
		return nil, err
	}
	old, err := s.readOriginAdmissionContext(ctx, gateway)
	if err != nil {
		return nil, err
	}
	next := OriginAdmission{SchemaVersion: task.SchemaVersion, RootID: s.RootID, GatewayID: gateway, RecoveryID: recovery, ExpectedGeneration: expected, Generation: generation}
	replay, err := validateOriginBarrierTransition(old, next)
	if err != nil {
		return nil, err
	}
	if replay {
		return s.verifyOriginAdmissionOwnedContext(ctx, old)
	}

	if old != nil {
		reused, scanErr := s.originGenerationUsedContext(ctx, gateway, generation, recovery)
		if scanErr != nil {
			return nil, scanErr
		}
		if reused {
			return nil, task.ErrRequestConflict
		}
	}
	return s.commitOriginAdmissionContext(ctx, old, next)
}

func (s *Store) requireOriginRootContext(ctx context.Context, expected string) error {
	if err := s.RequireConnectorEnabled(ctx); err != nil {
		return err
	}
	token, err := s.RootTokenContext(ctx)
	if err != nil {
		return err
	}
	if token != expected {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (s *Store) originGenerationUsedContext(ctx context.Context, gateway, generation, recovery string) (bool, error) {
	directory, err := s.openDir(s.originDirectory(gateway))
	if err != nil {
		return false, err
	}
	used := false
	err = scanDirectoryNames(func() error { return ctx.Err() }, directory, func(name string) error {
		if isStageFile(name) {
			return nil
		}
		var record OriginAdmission
		if readErr := s.readRecordContext(ctx, filepath.Join(s.originDirectory(gateway), name), &record); readErr != nil {
			return readErr
		}
		used = used || record.Generation == generation || record.RecoveryID == recovery
		return nil
	})
	return used, err
}

func (s *Store) commitOriginAdmissionContext(ctx context.Context, old *OriginAdmission, next OriginAdmission) (*OriginAdmission, error) {
	next.Sequence = 1
	if old != nil {
		next.Sequence = old.Sequence + 1
		data, err := marshalControlRecord(old)
		if err != nil {
			return nil, err
		}
		next.PreviousSHA256 = task.ComputeSHA256(data)
	}
	directory := s.originDirectory(next.GatewayID)
	if err := s.mkdirContext(ctx, directory, s.faultInjector); err != nil {
		return nil, err
	}
	data, err := marshalControlRecord(next)
	if err != nil {
		return nil, err
	}
	if err = s.commitConnectorRecordContext(ctx, directory, fmt.Sprintf("%020d.json", next.Sequence), data); err != nil {
		return nil, err
	}
	winner, err := s.readOriginAdmissionContext(ctx, next.GatewayID)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(winner, &next) {
		return nil, task.ErrInvariantFault
	}
	return winner, nil
}

func (s *Store) OriginInventoryContext(ctx context.Context, gateway, generation, cursor string, limit int, rootToken string) (result OriginInventory, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = OriginInventory{}
		}
	}()
	if err := validateOriginInventoryQuery(cursor, limit); err != nil {
		return result, err
	}
	if err := s.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return result, err
	}
	defer s.finishConnectorMaintenance(&resultErr)
	if err := s.requireOriginRootContext(ctx, rootToken); err != nil {
		return result, err
	}
	state, err := s.readOriginAdmissionContext(ctx, gateway)
	if err != nil {
		return result, err
	}
	if state == nil || state.Generation != generation {
		return result, task.ErrRequestConflict
	}
	entries, err := s.originMembershipContext(ctx, gateway)
	if err != nil {
		return result, err
	}
	return pageOriginInventory(*state, entries, cursor, limit)
}

func validateOriginInventoryQuery(cursor string, limit int) error {
	if limit < 1 || limit > 200 {
		return errors.New("inventory limit must be between 1 and 200")
	}
	if cursor != "" {
		return task.ValidateTaskID(cursor)
	}
	return nil
}

func pageOriginInventory(state OriginAdmission, entries []OriginInventoryEntry, cursor string, limit int) (result OriginInventory, resultErr error) {
	membership, err := identityToken(entries)
	if err != nil {
		return result, err
	}
	result.MembershipSHA256 = membership
	result.Entries = []OriginInventoryEntry{}
	for _, entry := range entries {
		if entry.TaskID <= cursor {
			continue
		}
		if len(result.Entries) == limit {
			result.NextCursor = result.Entries[len(result.Entries)-1].TaskID
			break
		}
		result.Entries = append(result.Entries, entry)
	}
	result.Complete = result.NextCursor == ""
	if result.Complete {
		result.CompletenessReceipt, err = originCompletenessToken(state, membership)
	}
	return result, err
}

func originCompletenessToken(state OriginAdmission, membership string) (string, error) {
	return identityToken(struct {
		State      OriginAdmission `json:"state"`
		Membership string          `json:"membership"`
	}{state, membership})
}

// OriginReopenContext rechecks canonical membership under mutation EX before
// enabling this generation. A stale scan can never reopen admission.
func (s *Store) OriginReopenContext(ctx context.Context, gateway, recovery, generation, receipt, rootToken string) (result *OriginAdmission, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	ancestors, err := s.beginConnectorMutationContext(ctx, true)
	if err != nil {
		return nil, err
	}
	defer s.finishConnectorAncestors(&resultErr, ancestors)
	if err = s.requireOriginRootContext(ctx, rootToken); err != nil {
		return nil, err
	}
	state, err := s.readOriginAdmissionContext(ctx, gateway)
	if err != nil {
		return nil, err
	}
	if state == nil || state.Generation != generation || state.RecoveryID != recovery {
		return nil, task.ErrRequestConflict
	}
	if state.AdmissionOpen {
		return s.verifyOpenedOriginRetryContext(ctx, state, receipt)
	}
	entries, err := s.originMembershipContext(ctx, gateway)
	if err != nil {
		return nil, err
	}
	membership, err := identityToken(entries)
	if err != nil {
		return nil, err
	}

	token, err := originCompletenessToken(*state, membership)
	if err != nil {
		return nil, err
	}
	if token != receipt {
		return nil, task.ErrRequestConflict
	}
	next := *state
	next.AdmissionOpen, next.MembershipSHA256 = true, membership
	return s.commitOriginAdmissionContext(ctx, state, next)
}

// Membership is immutable request/refusal identity only. Lifecycle changes do
// not invalidate inventory; incomplete/unreadable attribution prevents a proof.
func (s *Store) originMembershipContext(ctx context.Context, gateway string) ([]OriginInventoryEntry, error) {
	ids, err := s.scanPrivateDirectoryIDs(ctx, filepath.Join(s.Root, "tasks"), "task")
	if err != nil {
		return nil, err
	}
	entries := map[string]OriginInventoryEntry{}
	for _, id := range ids {
		entry, readErr := s.originTaskEntryContext(ctx, gateway, id)
		if readErr != nil {
			return nil, readErr
		}
		if entry != nil {
			entries[id] = *entry
		}
	}
	if err = s.appendOriginRefusalEntriesContext(ctx, gateway, entries); err != nil {
		return nil, err
	}
	result := make([]OriginInventoryEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	slices.SortFunc(result, func(a, b OriginInventoryEntry) int { return strings.Compare(a.TaskID, b.TaskID) })
	return result, nil
}

func (s *Store) originTaskEntryContext(ctx context.Context, gateway, id string) (*OriginInventoryEntry, error) {
	refusal, err := s.ReadNamespaceRefusalContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return s.originRefusalEntryContext(ctx, gateway, id, refusal)
	}
	td := &TaskDir{store: s, TaskID: id, Dir: filepath.Join(s.Root, "tasks", id)}
	spec, err := s.readBytesContext(ctx, filepath.Join(td.Dir, "task.json"), task.MaxControlRecordSize)
	if task.RecordAbsent(err) {
		return s.originTaskAfterAbsentContext(ctx, gateway, td)
	}
	if err != nil {
		return nil, err
	}
	return s.originTaskSpecContext(ctx, gateway, td, spec)
}

func (s *Store) originTaskAfterAbsentContext(ctx context.Context, gateway string, td *TaskDir) (result *OriginInventoryEntry, resultErr error) {
	namespace, err := s.openExistingLock(filepath.Join(s.Root, "task-namespace", td.TaskID+".lock"), LockLevelNone)
	if err != nil {
		return nil, fmt.Errorf("%w: unclassified task entry has no namespace owner: %w", task.ErrEvidenceFault, err)
	}
	if err = namespace.LockEXNonblockingContext(ctx); err != nil {
		return nil, errors.Join(err, namespace.closeIdle())
	}
	defer func() { resultErr = errors.Join(resultErr, s.closeNamespace(namespace)) }()
	refusal, err := s.ReadNamespaceRefusalContext(ctx, td.TaskID)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		verified, verifyErr := s.verifyNamespaceRefusalOwnedContext(ctx, td.TaskID, refusal)
		if verifyErr != nil {
			return nil, verifyErr
		}
		return originRefusalEntry(gateway, td.TaskID, verified), nil
	}
	spec, err := s.readBytesContext(ctx, filepath.Join(td.Dir, "task.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, fmt.Errorf("%w: unclassified task entry prevents complete inventory: %w", task.ErrEvidenceFault, err)
	}
	return s.originTaskSpecContext(ctx, gateway, td, spec)
}

func (s *Store) originTaskSpecContext(ctx context.Context, gateway string, td *TaskDir, spec []byte) (*OriginInventoryEntry, error) {
	request, err := td.decodeTaskRequest(spec)
	if err != nil {
		return nil, err
	}
	if request.Origin == nil || request.Origin.GatewayID != gateway {
		return nil, nil
	}
	receipt, err := connectorReceipt(request, spec)
	if err != nil {
		return nil, err
	}
	root, err := s.RootTokenContext(ctx)
	if err != nil {
		return nil, err
	}
	if receipt.RootToken != root {
		return nil, task.ErrIdentityMismatch
	}
	return &OriginInventoryEntry{Receipt: receipt, TaskID: td.TaskID, Resolution: "existing", Origin: request.Origin, SpecSHA256: receipt.SpecSHA256, BriefSHA256: receipt.BriefSHA256}, nil
}

func (s *Store) originRefusalEntryContext(ctx context.Context, gateway, id string, expected *NamespaceRefusal) (*OriginInventoryEntry, error) {
	refusal, err := s.verifyExistingNamespaceRefusalContext(ctx, id, expected)
	if err != nil {
		return nil, err
	}
	return originRefusalEntry(gateway, id, refusal), nil
}

// originRefusalEntry projects only an already verified canonical refusal.
func originRefusalEntry(gateway, id string, refusal *NamespaceRefusal) *OriginInventoryEntry {
	if refusal.Origin != nil && refusal.Origin.GatewayID != gateway {
		return nil
	}
	return &OriginInventoryEntry{TaskID: id, Resolution: "permanently_refused", Origin: refusal.Origin}
}

func (s *Store) appendOriginRefusalEntriesContext(ctx context.Context, gateway string, entries map[string]OriginInventoryEntry) error {
	directory, err := s.openDir(filepath.Join(s.Root, "task-refusals"))
	if directoryAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return scanDirectoryNames(func() error { return ctx.Err() }, directory, func(name string) error {
		if isStageFile(name) {
			return nil
		}
		id := strings.TrimSuffix(name, ".json")
		if name != id+".json" {
			return task.ErrEvidenceFault
		}
		refusal, readErr := s.ReadNamespaceRefusalContext(ctx, id)
		if readErr != nil {
			return readErr
		}
		if refusal == nil {
			return task.ErrEvidenceFault
		}
		entry, readErr := s.originRefusalEntryContext(ctx, gateway, id, refusal)
		if readErr != nil {
			return readErr
		}
		if entry != nil {
			entries[id] = *entry
		}
		return nil
	})
}

// Passive inventory borrows only the existing maintenance inode. It performs
// no filesystem probes, initialization, repair or task lifecycle transition.
func (r *ReadOnlyStore) OriginInventoryContext(ctx context.Context, gateway, generation, cursor string, limit int, rootToken string) (result OriginInventory, resultErr error) {
	resultErr = r.withBorrowedConnectorStoreContext(ctx, func(store *Store) error {
		var err error
		result, err = store.OriginInventoryContext(ctx, gateway, generation, cursor, limit, rootToken)
		return err
	})
	return result, resultErr
}

// ConnectorMaintenanceCleanupError retains the exact ancestor lease after a
// failed ancestor release. Release retries cleanup only; it never grants new work.
type ConnectorMaintenanceCleanupError struct {
	store     *Store
	ancestors *connectorAncestors
	cause     error
}

func (e *ConnectorMaintenanceCleanupError) Error() string         { return e.cause.Error() }
func (e *ConnectorMaintenanceCleanupError) Unwrap() error         { return e.cause }
func (e *ConnectorMaintenanceCleanupError) RetainedStore() *Store { return e.store }
func (e *ConnectorMaintenanceCleanupError) Release() error {
	if e.store == nil {
		return nil
	}
	if err := e.ancestors.release(); err != nil {
		return err
	}
	e.store = nil
	return nil
}

func (s *Store) finishConnectorMaintenance(resultErr *error) {
	s.finishConnectorAncestors(resultErr, &connectorAncestors{store: s})
}

// Publication retains the exact candidate through bounded absent/matching/
// conflicting/unreadable readback. A prior write error never establishes absence.
func (s *Store) commitConnectorRecordContext(ctx context.Context, directory, name string, data []byte) error {
	var writeErr, cleanupErr error
	for attempt := 0; attempt < 2; attempt++ {
		_, cleanupErr, writeErr = s.stageReaderOwnedContext(ctx, directory, name, bytes.NewReader(data), s.faultInjector)
		winner, readErr := s.readBytesContext(ctx, filepath.Join(directory, name), task.MaxControlRecordSize)
		if task.RecordAbsent(readErr) && attempt == 0 {
			continue
		}
		if readErr != nil {
			return errors.Join(writeErr, cleanupErr, readErr)
		}
		if !bytes.Equal(winner, data) {
			return errors.Join(writeErr, cleanupErr, task.ErrInvariantFault)
		}
		if err := s.barrierDir(directory); err != nil {
			return errors.Join(writeErr, cleanupErr, err)
		}
		return cleanupErr
	}
	return errors.Join(writeErr, cleanupErr, task.ErrUncertainDurability)
}

// ConnectorVerificationInjector extends per-store fault injection at the real
// existing-evidence persistence barrier. It changes no production policy.
type ConnectorVerificationInjector interface{ OnConnectorVerificationBarrier(path string) error }

func (s *Store) connectorVerificationHook(path string) error {
	if injector, ok := s.faultInjector.(ConnectorVerificationInjector); ok {
		return injector.OnConnectorVerificationBarrier(path)
	}
	return nil
}

// verifyNamespaceRefusalOwnedContext requires maintenance plus the existing
// namespace EX lease. It writes no record and syncs only existing evidence.
func (s *Store) verifyNamespaceRefusalOwnedContext(ctx context.Context, id string, expected *NamespaceRefusal) (*NamespaceRefusal, error) {
	if _, err := s.PinRootContext(ctx); err != nil {
		return nil, err
	}
	winner, err := s.ReadNamespaceRefusalContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if winner == nil || !reflect.DeepEqual(winner, expected) {
		return nil, task.ErrInvariantFault
	}
	path := filepath.Join(s.Root, "task-refusals", id+".json")
	file, err := s.openFile(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	err = errors.Join(s.connectorVerificationHook(path), ctx.Err())
	if err == nil {
		err = platformBarrierFile(file)
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return nil, err
	}
	for _, directory := range []string{filepath.Join(s.Root, "task-refusals"), filepath.Join(s.Root, "task-namespace"), s.Root} {
		if err = errors.Join(s.connectorVerificationHook(directory), ctx.Err()); err != nil {
			return nil, err
		}
		if err = s.barrierDir(directory); err != nil {
			return nil, err
		}
	}
	verified, err := s.ReadNamespaceRefusalContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(verified, winner) {
		return nil, task.ErrInvariantFault
	}
	return verified, nil
}

func (s *Store) verifyExistingNamespaceRefusalContext(ctx context.Context, id string, expected *NamespaceRefusal) (result *NamespaceRefusal, resultErr error) {
	namespace, err := s.openExistingLock(filepath.Join(s.Root, "task-namespace", id+".lock"), LockLevelNone)
	if err != nil {
		return nil, err
	}
	if err = namespace.LockEXNonblockingContext(ctx); err != nil {
		return nil, errors.Join(err, namespace.closeIdle())
	}
	defer func() { resultErr = errors.Join(resultErr, s.closeNamespace(namespace)) }()
	return s.verifyNamespaceRefusalOwnedContext(ctx, id, expected)
}

// ReadDurableNamespaceRefusalContext performs a persistence verification barrier
// under existing exclusion only. No lock/directory/refusal creation, logical
// repair, submission or provider work is authorized by passive resolution.
func (r *ReadOnlyStore) ReadDurableNamespaceRefusalContext(ctx context.Context, id string) (result *NamespaceRefusal, resultErr error) {
	resultErr = r.withBorrowedConnectorStoreContext(ctx, func(store *Store) (operationErr error) {
		if err := store.maintLock.LockSHNonblockingContext(ctx); err != nil {
			return err
		}
		defer store.finishConnectorMaintenance(&operationErr)
		candidate, err := store.ReadNamespaceRefusalContext(ctx, id)
		if err != nil || candidate == nil {
			return err
		}
		result, err = store.verifyExistingNamespaceRefusalContext(ctx, id, candidate)
		return err
	})
	if resultErr != nil {
		return nil, resultErr
	}
	return result, nil
}

// withBorrowedConnectorStoreContext lends existing lock/root authority only.
// Each operation owns its maintenance mode and retains failed dependent cleanup.
func (r *ReadOnlyStore) withBorrowedConnectorStoreContext(ctx context.Context, operation func(*Store) error) (resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return err
	}
	maintenance, err := openExistingStoreMaintenanceLock(ctx, r.store)
	if err != nil {
		return err
	}
	borrowed := &Store{Root: r.store.Root, RootID: r.store.RootID, rootHandle: r.store.rootHandle, predicates: r.store.predicates, maintLock: maintenance, faultInjector: r.store.faultInjector}
	maintenance.faultSource = borrowed.FaultInjector
	defer func() {
		if CleanupRetainsStore(resultErr, borrowed) {
			r.proofCleanup = &readOnlyProofCleanup{store: borrowed, cause: resultErr}
			return
		}
		resultErr = errors.Join(resultErr, maintenance.closeIdle())
	}()
	return operation(borrowed)
}

// InspectOriginAdmissionContext observes a registered origin under existing
// maintenance exclusion and verifies persistence. Absence never registers it.
func (r *ReadOnlyStore) InspectOriginAdmissionContext(ctx context.Context, gateway, rootToken string) (result *OriginAdmission, resultErr error) {
	resultErr = r.withBorrowedConnectorStoreContext(ctx, func(store *Store) (operationErr error) {
		if err := store.maintLock.LockSHNonblockingContext(ctx); err != nil {
			return err
		}
		defer store.finishConnectorMaintenance(&operationErr)
		if err := store.requireOriginRootContext(ctx, rootToken); err != nil {
			return err
		}
		state, err := store.readOriginAdmissionContext(ctx, gateway)
		if err != nil || state == nil {
			return err
		}
		result, err = store.verifyOriginAdmissionOwnedContext(ctx, state)
		return err
	})
	if resultErr != nil {
		return nil, resultErr
	}
	return result, nil
}

func (s *Store) verifyOriginAdmissionOwnedContext(ctx context.Context, expected *OriginAdmission) (*OriginAdmission, error) {
	directory := s.originDirectory(expected.GatewayID)
	path := filepath.Join(directory, fmt.Sprintf("%020d.json", expected.Sequence))
	file, err := s.openFile(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	err = errors.Join(s.connectorVerificationHook(path), ctx.Err())
	if err == nil {
		err = platformBarrierFile(file)
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return nil, err
	}
	for _, parent := range []string{directory, filepath.Dir(directory), s.Root} {
		if err = errors.Join(s.connectorVerificationHook(parent), ctx.Err()); err != nil {
			return nil, err
		}
		if err = s.barrierDir(parent); err != nil {
			return nil, err
		}
	}
	winner, err := s.readOriginAdmissionContext(ctx, expected.GatewayID)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(winner, expected) {
		return nil, task.ErrInvariantFault
	}
	return winner, nil
}

// TaskCreationNotAttemptedError describes THIS call's pre-commit phase only.
// It says nothing about another invocation's task existence or submission.
type TaskCreationNotAttemptedError struct{ cause error }

func (e *TaskCreationNotAttemptedError) Error() string { return e.cause.Error() }
func (e *TaskCreationNotAttemptedError) Unwrap() error { return e.cause }
func CreationNotAttempted(err error) bool {
	var phase *TaskCreationNotAttemptedError
	return errors.As(err, &phase)
}

func (s *Store) verifyOpenedOriginRetryContext(ctx context.Context, state *OriginAdmission, receipt string) (*OriginAdmission, error) {
	var prior OriginAdmission
	name := fmt.Sprintf("%020d.json", state.Sequence-1)
	if err := s.readRecordContext(ctx, filepath.Join(s.originDirectory(state.GatewayID), name), &prior); err != nil {
		return nil, err
	}
	token, err := originCompletenessToken(prior, state.MembershipSHA256)
	if err != nil {
		return nil, err
	}
	if token != receipt {
		return nil, task.ErrRequestConflict
	}
	return s.verifyOriginAdmissionOwnedContext(ctx, state)
}

func (s *Store) acquireCreationNamespaceContext(ctx context.Context, id string) (*LockFile, error) {
	record, err := readStoreRootRecord(ctx, s)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(record.RequiredFeatures, task.NamespaceAdmissionFeature) {
		return nil, nil
	}
	return s.taskNamespaceContext(ctx, id)
}

func (s *Store) retainTaskCreationContext(ctx context.Context, req *task.TaskRecord) (*ConnectorContextLease, error) {
	if req.ConnectorContext == nil {
		return nil, nil
	}
	token, err := ProjectToken(req.ConnectorContext.Project)
	if err != nil {
		return nil, err
	}
	return s.retainConnectorContext(ctx, req.ConnectorContext.RootToken, token, req.ConnectorContext.WorkspaceConstraint)
}

func (s *Store) validateCreatedConnectorContext(ctx context.Context, req *task.TaskRecord, lease *ConnectorContextLease) error {
	if err := s.validateConnectorRecordContext(ctx, req); err != nil {
		return err
	}
	if lease != nil {
		return lease.Validate(ctx)
	}
	return nil
}

func (td *TaskDir) validatePreparationMeta(data []byte, frozen *RecordedTaskIdentity) error {
	var meta task.MetaRecord
	if err := task.DecodeStrict(data, &meta); err != nil {
		return err
	}
	if err := task.ValidateMetaRecord(&meta); err != nil {
		return err
	}
	if meta.RootID != td.store.RootID || meta.TaskID != td.TaskID {
		return task.ErrIdentityMismatch
	}
	if frozen == nil {
		return nil
	}
	if meta.SpecSHA256 != task.ComputeSHA256(frozen.data) {
		return task.ErrRequestConflict
	}
	return nil
}

func validateOriginBarrierInput(gateway, recovery, expected, generation string) error {
	for _, id := range []string{gateway, recovery, generation} {
		if err := task.ValidateConnectorID(id); err != nil {
			return err
		}
	}
	if expected != "" {
		if err := task.ValidateConnectorID(expected); err != nil {
			return err
		}
	}
	if generation == expected {
		return task.ErrRequestConflict
	}
	return nil
}

func (s *Store) validateRefusedTaskFragmentsContext(ctx context.Context, id string) error {
	exists, err := s.TaskEntryExists(id)
	if err != nil || !exists {
		return err
	}
	td := &TaskDir{store: s, TaskID: id, Dir: filepath.Join(s.Root, "tasks", id)}
	request, err := td.recordedTaskIdentityContext(ctx)
	if err != nil && !task.RecordAbsent(err) {
		return err
	}
	if err = td.provePreparationOnlyContext(ctx, request); err != nil {
		return errors.Join(task.ErrInvariantFault, err)
	}
	return nil
}

// ConnectorReceipt derives immutable correlation directly from canonical task.json.
type ConnectorReceipt struct {
	ExecutionOptions    ConnectorExecutionOptions `json:"execution_options"`
	RootToken           string                    `json:"root_token"`
	ProjectToken        string                    `json:"project_token"`
	WorkspaceConstraint *task.WorkspaceConstraint `json:"workspace_constraint,omitempty"`
	TaskID              string                    `json:"task_id"`
	Origin              task.GatewayOrigin        `json:"origin"`
	SpecSHA256          string                    `json:"spec_sha256"`
	BriefSHA256         string                    `json:"brief_sha256"`
}

func (r *ReadOnlyStore) ReadConnectorReceiptContext(ctx context.Context, id string) (*ConnectorReceipt, error) {
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	data, err := r.store.readBytesContext(ctx, filepath.Join(r.store.Root, "tasks", id, "task.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	request, err := td.decodeTaskRequest(data)
	if err != nil {
		return nil, err
	}
	if request.Origin == nil || request.ConnectorContext == nil {
		return nil, task.ErrRequestConflict
	}
	root, err := r.store.RootTokenContext(ctx)
	if err != nil {
		return nil, err
	}
	if root != request.ConnectorContext.RootToken {
		return nil, task.ErrIdentityMismatch
	}
	return connectorReceipt(request, data)
}

// connectorReceipt projects one validated immutable request and its exact bytes.
// Project identity is frozen evidence; it must never observe the current path.
func connectorReceipt(request task.TaskRecord, data []byte) (*ConnectorReceipt, error) {
	if request.Origin == nil || request.ConnectorContext == nil {
		return nil, task.ErrRequestConflict
	}
	project, err := ProjectToken(request.ConnectorContext.Project)
	if err != nil {
		return nil, err
	}
	return &ConnectorReceipt{ExecutionOptions: connectorExecutionOptions(request), RootToken: request.ConnectorContext.RootToken, ProjectToken: project, WorkspaceConstraint: request.ConnectorContext.WorkspaceConstraint.Clone(), TaskID: request.TaskID, Origin: *request.Origin, SpecSHA256: task.ComputeSHA256(data), BriefSHA256: request.BriefSHA256}, nil
}

func (r *ReadOnlyStore) ConnectorTaskEntryExistsContext(ctx context.Context, id string) (bool, error) {
	if err := r.checkReadContext(ctx); err != nil {
		return false, err
	}
	exists, err := r.store.TaskEntryExists(id)
	return exists, errors.Join(err, ctx.Err())
}

// RecordedTaskIdentity retains one immutable task.json buffer and the request
// decoded from it. Only its Store owner reads the bytes; callers get projections.
// This is read-only attribution, never preparation or launch authority.
type RecordedTaskIdentity struct {
	request task.TaskRecord
	data    []byte
}

func (i *RecordedTaskIdentity) ConnectorReceipt() (*ConnectorReceipt, error) {
	return connectorReceipt(i.request, i.data)
}

func (s *Store) RecordedTaskIdentityContext(ctx context.Context, id string) (*RecordedTaskIdentity, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	td := &TaskDir{store: s, TaskID: id, Dir: filepath.Join(s.Root, "tasks", id)}
	return td.recordedTaskIdentityContext(ctx)
}

func (td *TaskDir) recordedTaskIdentityContext(ctx context.Context) (*RecordedTaskIdentity, error) {
	data, err := td.store.readBytesContext(ctx, filepath.Join(td.Dir, "task.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, err
	}
	request, err := td.decodeTaskRequest(data)
	if err != nil {
		return nil, err
	}
	return &RecordedTaskIdentity{request: request, data: data}, ctx.Err()
}

func (td *TaskDir) preparedIdentityCompleteContext(ctx context.Context, identity *RecordedTaskIdentity) error {
	digest, length, err := td.store.readDigestContext(ctx, filepath.Join(td.Dir, "brief.md"), task.MaxBriefSize)
	if err != nil {
		return err
	}
	_, _, _, _, err = td.loadAndValidatePreparedSetWithReader(briefIdentity{sha256: digest, length: length}, func(path string, limit int64) ([]byte, error) {
		if path == filepath.Join(td.Dir, "task.json") {
			if identity == nil {
				return nil, os.ErrNotExist
			}
			return identity.data, ctx.Err()
		}
		return td.store.readBytesContext(ctx, path, limit)
	})
	return errors.Join(err, ctx.Err())
}

// ConnectorClosureKind names canonical closure policy, independent of queue state.
type ConnectorClosureKind string

const (
	ConnectorClosureRegisteredNative      ConnectorClosureKind = "registered-native"
	ConnectorClosureRegisteredRecovered   ConnectorClosureKind = "registered-recovered"
	ConnectorClosureLegacyPreviousBoot    ConnectorClosureKind = "legacy-previous-boot"
	ConnectorClosureUnregisteredRecovered ConnectorClosureKind = "unregistered-recovered"
	ConnectorClosureNeverStarted          ConnectorClosureKind = "never-started"
)

// ConnectorExecutionClosure references an actual immutable lifetime/stop record.
// It is a verified observation, never a new task outcome or persisted ledger.
type ConnectorExecutionClosure struct {
	Kind            ConnectorClosureKind `json:"kind"`
	ReferenceSHA256 string               `json:"reference_sha256"`
}
type connectorClosureCandidate struct {
	kind      ConnectorClosureKind
	reference string
	names     []string
	raw       map[string][]byte
}

type ConnectorClosurePendingReason string

const (
	ConnectorClosureOwnerBusy       ConnectorClosurePendingReason = "owner_busy"
	ConnectorClosureMissingOwner    ConnectorClosurePendingReason = "missing_owner"
	ConnectorClosurePartialEvidence ConnectorClosurePendingReason = "partial_evidence"
	ConnectorClosureUnproven        ConnectorClosurePendingReason = "closure_unproven"
)

type ConnectorExecutionClosureObservation struct {
	Proof         *ConnectorExecutionClosure
	PendingReason ConnectorClosurePendingReason
}
type connectorClosurePendingError struct {
	reason ConnectorClosurePendingReason
	cause  error
}

func (e *connectorClosurePendingError) Error() string { return e.cause.Error() }
func (e *connectorClosurePendingError) Unwrap() error { return e.cause }
func connectorClosureObservationError(err error, missing ConnectorClosurePendingReason) error {
	if err == nil || CleanupFault(err) {
		return err
	}
	if task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) }) {
		return &connectorClosurePendingError{reason: ConnectorClosureOwnerBusy, cause: err}
	}
	if task.RecordAbsent(err) {
		return &connectorClosurePendingError{reason: missing, cause: err}
	}
	return err
}

func (r *ReadOnlyStore) ReadConnectorExecutionClosureContext(ctx context.Context, id string) (ConnectorExecutionClosureObservation, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return ConnectorExecutionClosureObservation{}, err
	}
	var proof *ConnectorExecutionClosure
	err := r.withBorrowedConnectorStoreContext(ctx, func(store *Store) error {
		var readErr error
		proof, readErr = store.readConnectorExecutionClosureContext(ctx, id)
		return readErr
	})
	return executionClosureObservation(proof, err)
}

// ReadTaskExecutionClosureContext observes canonical task closure with the
// same existing-inode exclusions as the read-only connector projection. It
// does not require a connector origin and never creates files or task state.
// A cleanup error retains this exact Store until its cleanup owner releases it.
func (s *Store) ReadTaskExecutionClosureContext(ctx context.Context, id string) (ConnectorExecutionClosureObservation, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return ConnectorExecutionClosureObservation{}, err
	}
	proof, err := s.readConnectorExecutionClosureContext(ctx, id)
	return executionClosureObservation(proof, err)
}

// ReadTaskPreparationOnlyContext classifies exact preparation fragments while
// holding existing task admission/run and any existing lifetime-owner inode.
// It creates no permanent refusal or closure evidence. The caller must already
// hold the runtime admission fence before treating this as transition-quiescent.
func (s *Store) ReadTaskPreparationOnlyContext(ctx context.Context, id string) (preparationOnly bool, resultErr error) {
	defer func() {
		if resultErr != nil {
			preparationOnly = false
		}
	}()
	if err := task.ValidateTaskID(id); err != nil {
		return false, err
	}
	if err := s.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return false, err
	}
	defer s.finishConnectorMaintenance(&resultErr)
	td, err := s.newTaskHandle(id, false)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	resultErr = td.withTaskObservationExclusionContext(ctx, false, true, func() error {
		evidence, readErr := LoadLifetimeEvidenceContext(ctx, td)
		if readErr != nil {
			return readErr
		}
		if evidence.Intent != nil || evidence.Registration != nil || evidence.Worker != nil || evidence.WorkerWait != nil || evidence.Termination != nil || len(evidence.Launches) != 0 {
			return nil
		}
		identity, readErr := td.recordedTaskIdentityContext(ctx)
		if readErr != nil && !task.RecordAbsent(readErr) {
			return readErr
		}
		if readErr = td.provePreparationOnlyContext(ctx, identity); readErr != nil {
			return readErr
		}
		preparationOnly = true
		return nil
	})
	return preparationOnly, resultErr
}

func executionClosureObservation(proof *ConnectorExecutionClosure, err error) (ConnectorExecutionClosureObservation, error) {
	if CleanupFault(err) {
		return ConnectorExecutionClosureObservation{}, err
	}
	var pending *connectorClosurePendingError
	if errors.As(err, &pending) && task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) || errors.Is(cause, os.ErrNotExist) }) {
		return ConnectorExecutionClosureObservation{PendingReason: pending.reason}, nil
	}
	if err != nil {
		return ConnectorExecutionClosureObservation{}, err
	}
	if proof == nil {
		return ConnectorExecutionClosureObservation{PendingReason: ConnectorClosureUnproven}, nil
	}
	return ConnectorExecutionClosureObservation{Proof: proof}, nil
}

func (s *Store) readConnectorExecutionClosureContext(ctx context.Context, id string) (proof *ConnectorExecutionClosure, resultErr error) {
	if err := s.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return nil, connectorClosureObservationError(err, ConnectorClosureMissingOwner)
	}
	defer s.finishConnectorMaintenance(&resultErr)
	td, err := s.newTaskHandle(id, false)
	if err != nil {
		return nil, connectorClosureObservationError(err, ConnectorClosureMissingOwner)
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	candidate, preflightErr := td.readConnectorClosureCandidateContext(ctx)
	if preflightErr != nil || candidate == nil {
		return nil, preflightErr
	}
	resultErr = td.withConnectorTaskExclusionContext(ctx, false, func() error {
		var readErr error
		proof, readErr = td.verifyConnectorClosureOwnedContext(ctx)
		return readErr
	})
	return proof, resultErr
}

func (td *TaskDir) verifyConnectorClosureOwnedContext(ctx context.Context) (*ConnectorExecutionClosure, error) {
	candidate, err := td.readConnectorClosureCandidateContext(ctx)
	if err != nil || candidate == nil {
		return nil, err
	}
	if err = td.verifyConnectorClosureFilesContext(ctx, candidate.names); err != nil {
		return nil, err
	}
	verified, err := td.readConnectorClosureCandidateContext(ctx)
	if err != nil {
		return nil, err
	}
	if verified == nil || verified.kind != candidate.kind || verified.reference != candidate.reference || !reflect.DeepEqual(verified.raw, candidate.raw) {
		return nil, task.ErrEvidenceFault
	}
	return &ConnectorExecutionClosure{Kind: candidate.kind, ReferenceSHA256: task.ComputeSHA256(verified.raw[verified.reference])}, nil
}

func (td *TaskDir) readConnectorClosureCandidateContext(ctx context.Context) (*connectorClosureCandidate, error) {
	if _, _, _, _, err := td.loadAndValidatePreparedSetContext(ctx); err != nil {
		return nil, connectorClosureObservationError(err, ConnectorClosurePartialEvidence)
	}
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return nil, err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil {
		return nil, err
	}
	var candidate *connectorClosureCandidate
	if closure.Closed() {
		candidate, err = nativeConnectorClosureCandidate(closure, evidence)
	} else {
		candidate, err = td.unlaunchedConnectorClosureCandidateContext(ctx)
	}
	if err != nil || candidate == nil {
		return nil, err
	}
	candidate.raw = map[string][]byte{}
	for _, name := range candidate.names {
		data, readErr := td.store.readBytesContext(ctx, filepath.Join(td.Dir, name), task.MaxControlRecordSize)
		if readErr != nil {
			return nil, errors.Join(task.ErrEvidenceFault, readErr)
		}
		candidate.raw[name] = data
	}
	return candidate, nil
}

func (td *TaskDir) unlaunchedConnectorClosureCandidateContext(ctx context.Context) (*connectorClosureCandidate, error) {
	eligible, err := td.unlaunchedTaskEvidenceContext(ctx)
	if err != nil || !eligible {
		return nil, err
	}
	name, digest, err := td.permanentLaunchRefusalContext(ctx)
	if err != nil {
		return nil, err
	}
	if digest == "" || name == "outcome.json" {
		return nil, nil
	}
	absent, err := td.taskRecordsAbsentContext(ctx, "result.txt", "publish.reject")
	if err != nil || !absent {
		return nil, err
	}
	return &connectorClosureCandidate{kind: ConnectorClosureNeverStarted, reference: name, names: []string{name}}, nil
}

func nativeConnectorClosureCandidate(closure task.LifetimeClosure, evidence task.LifetimeEvidence) (*connectorClosureCandidate, error) {
	kind, err := connectorNativeClosureKind(closure.Kind)
	if err != nil {
		return nil, err
	}
	var reference string
	switch closure.Reference {
	case task.LifetimeClosureReferenceWorkerWait:
		if evidence.WorkerWait == nil {
			return nil, task.ErrEvidenceFault
		}
		reference = filepath.Join(lifetimeDirectory, lifetimeWorkerWaitName)
	case task.LifetimeClosureReferenceTermination:
		if evidence.Termination == nil {
			return nil, task.ErrEvidenceFault
		}
		reference = filepath.Join(lifetimeDirectory, lifetimeTerminationName)
	case task.LifetimeClosureReferenceNone:
		return nil, task.ErrEvidenceFault
	}
	if reference == "" {
		return nil, task.ErrEvidenceFault
	}
	return &connectorClosureCandidate{kind: kind, reference: reference, names: connectorLifetimeEvidenceNames(evidence)}, nil
}

func connectorLifetimeEvidenceNames(evidence task.LifetimeEvidence) []string {
	names := []string{}
	for _, record := range []struct {
		name    string
		present bool
	}{
		{lifetimeIntentName, evidence.Intent != nil},
		{lifetimeRegistrationName, evidence.Registration != nil},
		{lifetimeWorkerName, evidence.Worker != nil},
		{lifetimeWorkerWaitName, evidence.WorkerWait != nil},
		{lifetimeTerminationName, evidence.Termination != nil},
	} {
		if record.present {
			names = append(names, filepath.Join(lifetimeDirectory, record.name))
		}
	}
	for _, launch := range evidence.Launches {
		names = append(names, filepath.Join(lifetimeDirectory, lifetimeLaunchName(launch.Sequence)))
	}
	return names
}

func connectorNativeClosureKind(kind task.LifetimeClosureKind) (ConnectorClosureKind, error) {
	switch kind {
	case task.LifetimeClosureRegisteredNative:
		return ConnectorClosureRegisteredNative, nil
	case task.LifetimeClosureRegisteredRecovered:
		return ConnectorClosureRegisteredRecovered, nil
	case task.LifetimeClosureLegacyPreviousBoot:
		return ConnectorClosureLegacyPreviousBoot, nil
	case task.LifetimeClosureUnregisteredRecovered:
		return ConnectorClosureUnregisteredRecovered, nil
	case task.LifetimeClosureOpen:
		return "", task.ErrEvidenceFault
	}
	return "", task.ErrEvidenceFault
}

func (td *TaskDir) verifyConnectorClosureFilesContext(ctx context.Context, names []string) error {
	parents := map[string]bool{td.Dir: true, filepath.Dir(td.Dir): true, td.store.Root: true}
	for _, name := range names {
		path := filepath.Join(td.Dir, name)
		file, err := td.store.openFile(path, os.O_RDONLY)
		if err != nil {
			return err
		}
		err = errors.Join(td.store.connectorVerificationHook(path), ctx.Err())
		if err == nil {
			err = platformBarrierFile(file)
		}
		if err = errors.Join(err, file.Close()); err != nil {
			return err
		}
		parents[filepath.Dir(path)] = true
	}
	for directory := range parents {
		if err := errors.Join(td.store.connectorVerificationHook(directory), ctx.Err()); err != nil {
			return err
		}
		if err := td.store.barrierDir(directory); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func validateOriginBarrierTransition(old *OriginAdmission, next OriginAdmission) (bool, error) {
	if old == nil {
		if next.ExpectedGeneration != "" {
			return false, task.ErrRequestConflict
		}
		return false, nil
	}
	if old.RecoveryID == next.RecoveryID {
		if old.Generation == next.Generation && old.ExpectedGeneration == next.ExpectedGeneration && !old.AdmissionOpen {
			return true, nil
		}
		return false, task.ErrRequestConflict
	}
	if old.Generation != next.ExpectedGeneration {
		return false, task.ErrRequestConflict
	}
	return false, nil
}

type ConnectorSelectionKind string

const (
	ConnectorSelectionExplicit      ConnectorSelectionKind = "explicit"
	ConnectorSelectionNativeDefault ConnectorSelectionKind = "native_default"
)

type ConnectorRequestedSelection struct {
	Kind  ConnectorSelectionKind `json:"kind"`
	Value string                 `json:"value,omitempty"`
}
type ConnectorExecutionOptions struct {
	Provider          string                      `json:"provider"`
	Permission        string                      `json:"permission"`
	BudgetNanoseconds int64                       `json:"budget_nanoseconds"`
	Model             ConnectorRequestedSelection `json:"model"`
	Effort            ConnectorRequestedSelection `json:"effort"`
}

func connectorRequestedSelection(value string) ConnectorRequestedSelection {
	if value == "" {
		return ConnectorRequestedSelection{Kind: ConnectorSelectionNativeDefault}
	}
	return ConnectorRequestedSelection{Kind: ConnectorSelectionExplicit, Value: value}
}

func connectorExecutionOptions(request task.TaskRecord) ConnectorExecutionOptions {
	return ConnectorExecutionOptions{Provider: request.Provider, Permission: request.Mode, BudgetNanoseconds: request.BudgetNanos, Model: connectorRequestedSelection(request.RequestedConfig.Model), Effort: connectorRequestedSelection(request.RequestedConfig.Effort)}
}

// ReadCommittedResultOutcomeContext uses the canonical publication/seal
// validators and cancellable streaming evidence checks, without an earlier
// uncancellable whole-payload inspection. Payload bytes are verified by Range.
func (r *ReadOnlyStore) ReadCommittedResultOutcomeContext(ctx context.Context, id string) (*task.OutcomeRecord, error) {
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	_, meta, spec, metaHash, err := td.loadAndValidatePreparedSetContext(ctx)
	if err != nil {
		return nil, err
	}
	var outcome task.OutcomeRecord
	if err = r.store.readRecordContext(ctx, filepath.Join(td.Dir, "outcome.json"), &outcome); err != nil {
		return nil, err
	}
	if err = td.validateOutcomeBinding(&outcome, meta, spec, metaHash); err != nil {
		return nil, err
	}
	if outcome.Verdict != task.VerdictCommitted {
		return nil, task.ErrRequestConflict
	}
	seal, err := td.readSealMetadata(meta.Predicate, spec, metaHash, func(path string, dst any) error { return td.store.readRecordContext(ctx, path, dst) })
	if err != nil {
		return nil, err
	}
	if seal.ManifestSHA256 != outcome.EvidenceSHA256 {
		return nil, task.ErrEvidenceFault
	}
	_, evidence, err := td.rawManifestContext(ctx)
	if err != nil {
		return nil, err
	}
	if evidence != outcome.EvidenceSHA256 {
		return nil, task.ErrEvidenceFault
	}
	return &outcome, ctx.Err()
}
