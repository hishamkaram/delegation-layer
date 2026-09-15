package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Store manages the root-level delegation store and task directories.
type Store struct {
	predicates    predicate.Registry
	Root          string
	RootID        string
	rootHandle    *os.Root
	maintLock     *LockFile
	faultInjector FaultInjector
	mu            sync.Mutex
	closed        bool
}

func closeLockQuietly(l *LockFile) {
	if l == nil {
		return
	}
	if err := l.Close(); err != nil {
		return
	}
}

func closeRootHandleQuietly(r *os.Root) {
	if r == nil {
		return
	}
	if err := r.Close(); err != nil {
		return
	}
}

// SetFaultInjector sets the per-store fault injection hook.
func (s *Store) SetFaultInjector(injector FaultInjector) {
	s.faultInjector = injector
}

// FaultInjector returns the current fault injector.
func (s *Store) FaultInjector() FaultInjector {
	return s.faultInjector
}

func (s *Store) createNewRoot(cleanRoot string) error {
	rootID, idErr := task.NewRootID()
	if idErr != nil {
		return fmt.Errorf("generating root ID: %w", idErr)
	}
	rootRec := task.RootRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, mErr := marshalControlRecord(rootRec)
	if mErr != nil {
		return fmt.Errorf("marshaling root.json: %w", mErr)
	}
	committed, cleanupErr, cErr := s.stageAndCommit(cleanRoot, "root.json", data, nil)
	if cErr != nil && !committed {
		return fmt.Errorf("committing root.json: %w", cErr)
	}
	if cleanupErr != nil {
		return fmt.Errorf("cleanup root.json: %w", cleanupErr)
	}
	s.RootID = rootID
	return nil
}

func (s *Store) loadExistingRoot(rootJSONPath string) error {
	f, oErr := s.openFile(rootJSONPath, os.O_RDONLY)
	if oErr != nil {
		return fmt.Errorf("opening root.json: %w", oErr)
	}
	content, rErr := task.ReadControlRecord(f)
	closeFileQuietly(f)
	if rErr != nil {
		return fmt.Errorf("reading root.json: %w", rErr)
	}
	var rootRec task.RootRecord
	if dErr := task.DecodeStrict(content, &rootRec); dErr != nil {
		return fmt.Errorf("decoding root.json: %w", dErr)
	}
	if vErr := task.ValidateRootRecord(&rootRec); vErr != nil {
		return fmt.Errorf("validating root record: %w", vErr)
	}
	s.RootID = rootRec.RootID
	return nil
}

// InitStore initializes a state store at rootPath with root.json and .maintenance.lock.
func InitStore(rootPath string) (*Store, error) {
	return InitStoreWithPredicates(rootPath, predicate.Default())
}

func InitStoreWithPredicates(rootPath string, registry predicate.Registry) (*Store, error) {
	if !filepath.IsAbs(rootPath) {
		return nil, fmt.Errorf("state root must be an absolute path: %s", rootPath)
	}
	cleanRoot := filepath.Clean(rootPath)

	// Persist missing parents up to already existing ancestor
	if err := createDirectoriesWithAncestorBarrier(cleanRoot, nil); err != nil {
		return nil, fmt.Errorf("creating state root %s: %w", cleanRoot, err)
	}

	canonicalRoot, canonicalErr := filepath.EvalSymlinks(cleanRoot)
	if canonicalErr != nil {
		return nil, canonicalErr
	}
	cleanRoot = canonicalRoot
	rootHandle, err := os.OpenRoot(cleanRoot)
	if err != nil {
		return nil, fmt.Errorf("opening root handle for %s: %w", cleanRoot, err)
	}

	maintLockPath := filepath.Join(cleanRoot, ".maintenance.lock")
	tempStore := &Store{Root: cleanRoot, rootHandle: rootHandle}
	rootExists, rootErr := tempStore.entryExists(filepath.Join(cleanRoot, "root.json"))
	if rootErr != nil {
		closeRootHandleQuietly(rootHandle)
		return nil, rootErr
	}
	openLock := tempStore.openLock
	if rootExists {
		openLock = tempStore.openExistingLock
	}
	mLock, err := openLock(maintLockPath, LockLevelMaintenance)
	if err != nil {
		closeRootHandleQuietly(rootHandle)
		return nil, fmt.Errorf("initializing maintenance lock: %w", err)
	}

	s := &Store{
		predicates: registry,
		Root:       cleanRoot,
		rootHandle: rootHandle,
		maintLock:  mLock,
	}

	s.maintLock.faultSource = s.FaultInjector

	// Mandatory filesystem support and policy check before authority
	if pErr := s.ProbeFilesystemSupport(); pErr != nil {
		closeLockQuietly(mLock)
		closeRootHandleQuietly(rootHandle)
		return nil, fmt.Errorf("filesystem support check failed: %w", pErr)
	}

	rootJSONPath := filepath.Join(cleanRoot, "root.json")
	exists, statErr := s.exists(rootJSONPath)
	if statErr != nil {
		closeLockQuietly(mLock)
		closeRootHandleQuietly(rootHandle)
		return nil, statErr
	}
	if !exists {
		if err := s.createNewRoot(cleanRoot); err != nil {
			closeLockQuietly(mLock)
			closeRootHandleQuietly(rootHandle)
			return nil, err
		}
	} else {
		if err := s.loadExistingRoot(rootJSONPath); err != nil {
			closeLockQuietly(mLock)
			closeRootHandleQuietly(rootHandle)
			return nil, err
		}
	}

	return s, nil
}

// OpenStore opens an existing store at rootPath.
func OpenStore(rootPath string) (*Store, error) {
	return OpenStoreWithPredicates(rootPath, predicate.Default())
}

func OpenStoreWithPredicates(rootPath string, registry predicate.Registry) (*Store, error) {
	s, err := openStoreMetadata(rootPath, registry)
	if err != nil {
		return nil, err
	}
	if err = s.ProbeFilesystemSupport(); err != nil {
		return nil, errors.Join(fmt.Errorf("filesystem support check failed: %w", err), s.Close())
	}
	return s, nil
}

// OpenStoreWithPredicatesContext opens an existing store and bounds its probe
// by the caller's observation lifetime. It never initializes a missing store.
func OpenStoreWithPredicatesContext(ctx context.Context, rootPath string, registry predicate.Registry) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := openStoreMetadata(rootPath, registry)
	if err != nil {
		return nil, err
	}
	if err = s.ProbeFilesystemSupportContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("filesystem support check failed: %w", err), s.Close())
	}
	return s, nil
}

func openStoreMetadata(rootPath string, registry predicate.Registry) (*Store, error) {
	if !filepath.IsAbs(rootPath) {
		return nil, fmt.Errorf("state root must be an absolute path: %s", rootPath)
	}
	cleanRoot := filepath.Clean(rootPath)

	canonicalRoot, canonicalErr := filepath.EvalSymlinks(cleanRoot)
	if canonicalErr != nil {
		return nil, canonicalErr
	}
	cleanRoot = canonicalRoot
	rootHandle, err := os.OpenRoot(cleanRoot)
	if err != nil {
		return nil, fmt.Errorf("opening root handle for %s: %w", cleanRoot, err)
	}

	rootJSONPath := filepath.Join(cleanRoot, "root.json")
	tempStore := &Store{Root: cleanRoot, rootHandle: rootHandle}
	f, oErr := tempStore.openFile(rootJSONPath, os.O_RDONLY)
	if oErr != nil {
		closeRootHandleQuietly(rootHandle)
		return nil, fmt.Errorf("opening root.json: %w", oErr)
	}
	content, rErr := task.ReadControlRecord(f)
	closeFileQuietly(f)
	if rErr != nil {
		closeRootHandleQuietly(rootHandle)
		return nil, fmt.Errorf("reading root.json: %w", rErr)
	}

	var rootRec task.RootRecord
	if dErr := task.DecodeStrict(content, &rootRec); dErr != nil {
		closeRootHandleQuietly(rootHandle)
		return nil, fmt.Errorf("decoding root.json: %w", dErr)
	}
	if vErr := task.ValidateRootRecord(&rootRec); vErr != nil {
		closeRootHandleQuietly(rootHandle)
		return nil, fmt.Errorf("validating root record: %w", vErr)
	}

	maintLockPath := filepath.Join(cleanRoot, ".maintenance.lock")
	mLock, lErr := tempStore.openExistingLock(maintLockPath, LockLevelMaintenance)
	if lErr != nil {
		closeRootHandleQuietly(rootHandle)
		return nil, fmt.Errorf("opening maintenance lock: %w", lErr)
	}

	s := &Store{
		predicates: registry,
		Root:       cleanRoot,
		RootID:     rootRec.RootID,
		rootHandle: rootHandle,
		maintLock:  mLock,
	}

	s.maintLock.faultSource = s.FaultInjector

	return s, nil
}

// Close closes the store's lock files and root handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.maintLock.closeIdle(); err != nil {
		return err
	}
	s.closed = true
	return s.rootHandle.Close()
}

// ProbeFilesystemSupport verifies platform filesystem policy, hardlinks,
// file/directory sync barriers, and flock support.
func (s *Store) ProbeFilesystemSupport() (resultErr error) {
	if err := s.maintLock.LockSHNonblocking(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.maintLock.Unlock()) }()
	return s.probeFilesystemSupport(context.Background())
}

// ProbeFilesystemSupportContext waits for maintenance access only within ctx.
// Once a probe file is opened, its writes, barriers and cleanup remain owned.
func (s *Store) ProbeFilesystemSupportContext(ctx context.Context) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.maintLock.LockSH(ctx); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.maintLock.Unlock()) }()
	return s.probeFilesystemSupport(ctx)
}

func (s *Store) probeFilesystemSupport(ctx context.Context) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkPlatformFilesystemPolicy(s.Root); err != nil {
		return err
	}

	probeID, idErr := task.NewRandomID()
	if idErr != nil {
		return fmt.Errorf("filesystem probe id: %w", idErr)
	}
	probeDir := filepath.Join(s.Root, ".probe", probeID)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.mkdir(probeDir, nil); err != nil {
		return fmt.Errorf("filesystem probe mkdir: %w", err)
	}
	defer func() {
		rel, err := s.relative(probeDir)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		resultErr = errors.Join(resultErr, s.rootHandle.RemoveAll(rel), s.barrierDir(filepath.Dir(probeDir)))
	}()

	return s.probeFilesystemOperations(ctx, probeDir)
}

func (s *Store) probeFilesystemOperations(ctx context.Context, probeDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fPath := filepath.Join(probeDir, "probe.file")
	f, err := s.openFile(fPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return fmt.Errorf("filesystem probe open: %w", err)
	}
	if _, err := f.Write([]byte("probe")); err != nil {
		closeFileQuietly(f)
		return fmt.Errorf("filesystem probe write: %w", err)
	}
	if err := platformBarrierFile(f); err != nil {
		closeFileQuietly(f)
		return fmt.Errorf("filesystem probe file barrier: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("filesystem probe close: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	linkPath := filepath.Join(probeDir, "probe.link")
	if err := s.link(fPath, linkPath); err != nil {
		return fmt.Errorf("filesystem probe hardlink unsupported: %w", err)
	}

	if err := s.barrierDir(probeDir); err != nil {
		return fmt.Errorf("filesystem probe dir barrier: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	// Verify flock support on probe lock
	probeLockPath := filepath.Join(probeDir, ".probe.lock")
	pLock, lErr := s.openLock(probeLockPath, LockLevelNone)
	if lErr != nil {
		return fmt.Errorf("filesystem probe lock open: %w", lErr)
	}
	if shErr := pLock.LockSH(ctx); shErr != nil {
		closeLockQuietly(pLock)
		return fmt.Errorf("filesystem probe lock SH: %w", shErr)
	}
	if uErr := pLock.Unlock(); uErr != nil {
		closeLockQuietly(pLock)
		return fmt.Errorf("filesystem probe unlock: %w", uErr)
	}
	closeLockQuietly(pLock)

	return nil
}
