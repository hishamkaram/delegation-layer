package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The scheduler borrows an already-running queue. The parent test owns its
// native lifetime; the scheduler has no authority to start or replace it.
func fixturePueue(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	client, reachable, err := pueue.ProbePrivate(ctx, os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED"), os.Getenv("DL_HOST_ROOT"), pueue.Options{ObservationTimeout: 5 * time.Second, WaitForPendingClose: true})
	if err != nil || client == nil {
		return errors.Join(err, errors.New("parent-owned fixture queue is unavailable"))
	}
	if !reachable {
		return errors.Join(errors.New("parent-owned fixture queue is not ready"), client.Close())
	}
	return client.Close()
}

func TestHostEOFReleasesEWithExistingParentOwnedPueue(t *testing.T) {
	clientPath, daemonPath := os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED")
	if clientPath == "" || daemonPath == "" {
		t.Skip("native Pueue paths are not configured")
	}
	fixture := nativeQueue(t)
	config := fixture.config
	markers := config.Directory
	fixture.bind(t, clientPath, daemonPath)
	process := startHostFixture(t, config)
	query := queryFor(t, config)
	ready := eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("host endpoint: %t %v", present, err)
	}
	pinLostHostEndpoint(t, process, epoch.Endpoint)
	assertCapturedContext(t, markers, ready.Host.PID) // Written after queue readiness is observed.
	client, reachable, err := pueue.ProbePrivate(t.Context(), clientPath, daemonPath, config.Store.Root, pueue.Options{ObservationTimeout: 5 * time.Second, WaitForPendingClose: true})
	if err != nil || !reachable || client == nil {
		t.Fatalf("existing task queue not ready: %t %v", reachable, err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = process.control.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	receive(t, process.done)
	if ready.Child == nil {
		t.Fatal("ready resident identity is absent")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err = awaitIdentityAbsent(ctx, *ready.Child); err != nil {
		t.Fatalf("resident remained after host EOF: %v", err)
	}
	assertLeasesFree(t, config)
	select {
	case <-fixture.waits[0]:
		t.Fatal("parent-owned daemon exited with the scheduler")
	default:
	}
	if err = client.Ready(ctx); err != nil {
		t.Fatalf("task queue did not remain alive after host/resident exit: %v", err)
	}
}

// This fixture alone grants test-local start/publication authority for its pin.
// Pueue owns the sole native Wait; waits retain its actual completion handles.
type nativeQueueFixture struct {
	store          *taskdir.Store
	pin            taskdir.RootPin
	client         *pueue.Client
	bindErr        error
	waits          []<-chan struct{}
	closed         bool
	startAttempted bool
	config         Config
	parent         *os.Root
	name           string
	info           os.FileInfo
}

// A native queue must not use automatic t.TempDir deletion: retain its unique
// root and evidence when canonical shutdown cannot establish queue closure.
func nativeQueue(t *testing.T) *nativeQueueFixture {
	t.Helper()
	directory := nativeQueueDirectory(t)
	owned := false
	t.Cleanup(func() {
		if !owned {
			t.Errorf("native queue setup could not pin its directory; retained %s", directory)
		}
	})
	parent, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		t.Fatal(err)
	}
	info, err := parent.Lstat(filepath.Base(directory))
	if err != nil {
		t.Fatal(errors.Join(err, parent.Close()))
	}
	fixture := &nativeQueueFixture{parent: parent, name: filepath.Base(directory), info: info}
	t.Cleanup(func() { fixture.cleanup(t) })
	owned = true
	fixture.store, err = taskdir.InitStore(filepath.Join(directory, "state"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.pin, err = fixture.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fixture.config, err = fixtureConfig(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	markers := filepath.Join(directory, "markers")
	if err = os.Mkdir(markers, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.config.Directory = markers
	fixture.config.Environment = append(fixture.config.Environment, "DL_HOST_MODE=pueue", "DL_HOST_MARKERS="+markers, "DL_HOST_VISIBLE=")
	return fixture
}

func nativeQueueDirectory(t *testing.T) string {
	t.Helper()
	// Darwin's default TMPDIR can leave too little room for the private queue
	// socket. A short fixture directly under native HOME preserves the caller's
	// environment and avoids changing provider or global temporary settings.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".hq-")
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func (f *nativeQueueFixture) bind(t *testing.T, clientPath, daemonPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	f.bindErr = f.prepare(ctx, clientPath, daemonPath, f)
	if f.bindErr != nil || f.client == nil || len(f.waits) != 1 {
		t.Fatalf("parent queue bootstrap: waits=%d error=%v", len(f.waits), f.bindErr)
	}
	if err := f.client.Ready(ctx); err != nil {
		t.Fatal(err)
	}
}

// Prepare before Start retains the exact client even if publication fails after
// native birth. Cleanup must not rediscover an unpublished generation.
func (f *nativeQueueFixture) prepare(ctx context.Context, clientPath, daemonPath string, generation pueue.PrivateGenerationGuard) error {
	return pueue.WithBootstrapLockContext(ctx, f.store.Root, func(locked context.Context) error {
		var err error
		f.client, err = pueue.PreparePrivateWithLock(locked, clientPath, daemonPath, f.store.Root, pueue.Options{
			ObservationTimeout: 5 * time.Second, WaitForPendingClose: true,
			PrivateGenerationGuard: generation, DaemonStartGuard: f, DaemonWaitOwner: f,
		})
		if err != nil {
			return err
		}
		return f.client.StartPrivate(locked, f.store.Root)
	})
}

func (f *nativeQueueFixture) guard(ctx context.Context, root string, action func() error) error {
	if ctx == nil || action == nil || f.closed || f.store == nil || root != f.pin.Root() {
		return ErrIdentity
	}
	pin, err := f.store.PinRootContext(ctx)
	if err != nil || !pin.Equal(f.pin) {
		return errors.Join(ErrIdentity, err)
	}
	return action()
}

func (f *nativeQueueFixture) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return f.guard(ctx, root, func() error {
		if f.startAttempted {
			return ErrIdentity
		}
		f.startAttempted = true
		return start()
	})
}

func (f *nativeQueueFixture) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	return f.guard(ctx, root, commit)
}

func (f *nativeQueueFixture) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	if root != f.pin.Root() || pid <= 0 || done == nil {
		return ErrIdentity
	}
	f.waits = append(f.waits, done)
	if len(f.waits) != 1 {
		return ErrIdentity
	}
	return nil
}

func (f *nativeQueueFixture) closeQueue(ctx context.Context) (resultErr error) {
	if f.closed {
		return nil
	}
	defer func() {
		if resultErr == nil {
			f.closed = true
		}
	}()
	client := f.client
	if client != nil {
		defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(f.bindErr)) }()
		if len(f.waits) != 0 {
			if err := client.Shutdown(ctx); err != nil {
				return err
			}
			if err := client.WaitUnavailable(ctx); err != nil {
				return err
			}
		}
	}
	for _, done := range f.waits {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (f *nativeQueueFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
	defer cancel()
	closeErr := f.closeQueue(ctx)
	if f.store != nil {
		closeErr = errors.Join(closeErr, f.store.Close())
	}
	if closeErr == nil {
		current, statErr := f.parent.Lstat(f.name)
		if statErr == nil && os.SameFile(current, f.info) {
			statErr = f.parent.RemoveAll(f.name)
		} else if statErr == nil {
			statErr = ErrIdentity
		}
		closeErr = statErr
	}
	if closeErr != nil {
		t.Errorf("native queue closure/removal unverified; retain %s: %v", filepath.Join(f.parent.Name(), f.name), closeErr)
	}
	if err := f.parent.Close(); err != nil {
		t.Error(err)
	}
}

// A real post-Start publication failure must retain its original client and
// canonical completion, then settle both before the root may be removed.
func TestNativeQueueCleanupAfterPublicationFailure(t *testing.T) {
	clientPath, daemonPath := os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED")
	if clientPath == "" || daemonPath == "" {
		t.Skip("native Pueue paths are not configured")
	}
	f := nativeQueue(t)
	fault := errors.New("fixture publication denied after native Start")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	f.bindErr = f.prepare(ctx, clientPath, daemonPath, nativeQueuePublicationFault{f, fault})
	if !errors.Is(f.bindErr, fault) || f.client == nil || len(f.waits) != 1 {
		t.Fatalf("post-Start failure lost owned client/Wait: waits=%d err=%v", len(f.waits), f.bindErr)
	}
	done := f.waits[0]
	select {
	case <-done:
		t.Fatal("fixture daemon exited before cleanup exercised its owner")
	default:
	}
	if err := f.closeQueue(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("cleanup returned before the actual daemon Wait")
	}
}

type nativeQueuePublicationFault struct {
	fixture *nativeQueueFixture
	cause   error
}

func (g nativeQueuePublicationFault) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	return g.fixture.guard(ctx, root, func() error {
		if len(g.fixture.waits) != 0 {
			return g.cause
		}
		return commit()
	})
}
