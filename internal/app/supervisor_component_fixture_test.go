package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type componentDaemonLifetime uint8

const (
	componentFiniteDaemon componentDaemonLifetime = iota + 1
	componentPersistentDaemon
)

const componentFixtureCleanupTimeout = 10 * time.Second

type componentSupervisorCall struct{ kind string }

type componentDaemonCompletion struct {
	pid    int
	done   <-chan struct{}
	client *pueue.Client
}

type componentSupervisorEndpoint struct {
	listener net.Listener
	done     chan struct{}
}

// This owner supplies explicit ports only to fake-CLI component setup. It
// proves no native placement and is never installed in production Dependencies.
// The test joins its concurrent work before cleanup; this owner then refuses
// further callbacks and closes actual daemon waits before their borrowers.
type componentSupervisorFixture struct {
	mu            sync.Mutex
	base          string
	root          string
	directory     *os.File
	lifetime      componentDaemonLifetime
	closing       bool
	calls         []componentSupervisorCall
	waits         []componentDaemonCompletion
	pending       []<-chan struct{}
	clients       []*pueue.Client
	store         *taskdir.Store
	rootDirectory *os.File
	tasks         []*taskdir.TaskDir
	endpoints     []*componentSupervisorEndpoint
	closeOnce     sync.Once
	closeErr      error
}

func newComponentSupervisorFixture(t *testing.T, lifetime componentDaemonLifetime) *componentSupervisorFixture {
	t.Helper()
	return newComponentSupervisorFixtureIn(t, lifetime, "")
}

func newComponentSupervisorFixtureIn(t *testing.T, lifetime componentDaemonLifetime, parent string) *componentSupervisorFixture {
	t.Helper()
	owner := &componentSupervisorFixture{lifetime: lifetime}
	t.Cleanup(func() { owner.cleanup(t) })
	base, err := os.MkdirTemp(parent, "dl-sc-")
	owner.base = base
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		t.Fatal(err)
	}
	owner.base = canonical
	owner.root = filepath.Join(owner.base, "state")
	owner.directory, err = os.Open(owner.base)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func (f *componentSupervisorFixture) setupOptions(options pueue.Options) pueue.Options {
	options.DaemonStartGuard = f
	options.PrivateGenerationGuard = f
	options.DaemonWaitOwner = f
	options.WaitForPendingClose = true
	return options
}

func (f *componentSupervisorFixture) bindPrivate(ctx context.Context, clientPath, daemonPath string, options pueue.Options) (*pueue.Client, error) {
	before := len(f.daemonWaits())
	client, err := pueue.BindPrivate(ctx, clientPath, daemonPath, f.root, f.setupOptions(options))
	f.ownClient(client, err)
	f.ownDaemonClients(client, before)
	return client, err
}

func (f *componentSupervisorFixture) ownDaemonClients(client *pueue.Client, first int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := first; index < len(f.waits); index++ {
		f.waits[index].client = client
	}
}

func (f *componentSupervisorFixture) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return f.invoke(ctx, root, "start", start)
}

func (f *componentSupervisorFixture) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	return f.invoke(ctx, root, "commit", commit)
}

func (f *componentSupervisorFixture) invoke(ctx context.Context, root, kind string, callback func() error) error {
	if ctx == nil || callback == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.closing || f.root == "" || root != f.root {
		return pueue.ErrInfrastructureOwnerRequired
	}
	f.calls = append(f.calls, componentSupervisorCall{kind: kind})
	return callback()
}

func (f *componentSupervisorFixture) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if root != f.root || pid <= 0 || done == nil {
		return os.ErrInvalid
	}
	// Start already happened. Retain its actual sole-Wait completion even if
	// cleanup has begun; rejecting registration cannot unwind native Start.
	f.waits = append(f.waits, componentDaemonCompletion{pid: pid, done: done})
	return nil
}

func (f *componentSupervisorFixture) ownClient(client *pueue.Client, cause error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if client != nil && !slices.Contains(f.clients, client) {
		f.clients = append(f.clients, client)
	}
	var inFlight *pueue.InFlightError
	if errors.As(cause, &inFlight) && inFlight.Pending != nil {
		f.pending = append(f.pending, inFlight.Pending.Done())
	}
}

func (f *componentSupervisorFixture) ownStore(t *testing.T, store *taskdir.Store) {
	t.Helper()
	if f.store == store && f.rootDirectory != nil {
		return
	}
	f.store = store
	if store == nil {
		return
	}
	// Older fixture creators transfer their Store from t.Cleanup, after the
	// test context ends. This finite pin capture owns cleanup, not admission.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), componentFixtureCleanupTimeout)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.rootDirectory, err = os.Open(pin.Root())
	if err != nil {
		t.Fatal(err)
	}
}

func (f *componentSupervisorFixture) ownTask(td *taskdir.TaskDir) {
	if td != nil && !slices.Contains(f.tasks, td) {
		f.tasks = append(f.tasks, td)
	}
}

func (f *componentSupervisorFixture) listen(t *testing.T, endpoint string) {
	t.Helper()
	path, unix := strings.CutPrefix(endpoint, "unix:")
	if !unix || !filepath.IsAbs(path) {
		t.Fatal("component supervisor requires an absolute Unix endpoint")
	}
	owned := &componentSupervisorEndpoint{}
	f.endpoints = append(f.endpoints, owned)
	var err error
	owned.listener, err = net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	owned.done = make(chan struct{})
	go func() {
		defer close(owned.done)
		for {
			connection, acceptErr := owned.listener.Accept()
			if acceptErr != nil {
				return
			}
			if closeErr := connection.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	}()
}

func (f *componentSupervisorFixture) cleanup(t *testing.T) {
	t.Helper()
	f.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), componentFixtureCleanupTimeout)
		defer cancel()
		f.closeErr = f.close(ctx)
	})
	if f.closeErr != nil {
		t.Errorf("retained component supervisor fixture %s: %v", f.base, f.closeErr)
	}
}

func (f *componentSupervisorFixture) close(ctx context.Context) error {
	f.mu.Lock()
	f.closing = true
	waits := slices.Clone(f.waits)
	f.mu.Unlock()
	if f.base == "" {
		return nil
	}
	if err := f.checkPins(ctx); err != nil {
		return err
	}
	for _, done := range f.pending {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("component control Wait unresolved: %w", ctx.Err())
		}
	}
	if err := f.stopDaemons(ctx, waits); err != nil {
		return err
	}
	if err := awaitComponentDaemons(ctx, waits); err != nil {
		return err
	}
	if err := f.closeBorrowers(); err != nil {
		return err
	}
	if err := f.checkDirectory(); err != nil {
		return err
	}
	if err := f.directory.Close(); err != nil {
		return err
	}
	return os.RemoveAll(f.base)
}

func (f *componentSupervisorFixture) daemonWaits() []componentDaemonCompletion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.waits)
}

func (f *componentSupervisorFixture) stopDaemons(ctx context.Context, waits []componentDaemonCompletion) error {
	if f.lifetime != componentPersistentDaemon || componentDaemonsClosed(waits) {
		return nil
	}
	return pueue.WithBootstrapLockContext(ctx, f.root, func(locked context.Context) error {
		for _, wait := range waits {
			if err := stopComponentDaemon(locked, wait); err != nil {
				return err
			}
		}
		return nil
	})
}

func componentDaemonsClosed(waits []componentDaemonCompletion) bool {
	for _, wait := range waits {
		select {
		case <-wait.done:
		default:
			return false
		}
	}
	return true
}

func stopComponentDaemon(ctx context.Context, wait componentDaemonCompletion) error {
	select {
	case <-wait.done:
		return nil
	default:
	}
	if wait.client == nil {
		return fmt.Errorf("started component daemon %d has no shutdown client", wait.pid)
	}
	if err := wait.client.Shutdown(ctx); err != nil {
		return err
	}
	return wait.client.WaitUnavailable(ctx)
}

func awaitComponentDaemons(ctx context.Context, waits []componentDaemonCompletion) error {
	for _, wait := range waits {
		select {
		case <-wait.done:
		case <-ctx.Done():
			return fmt.Errorf("component daemon %d Wait unresolved: %w", wait.pid, ctx.Err())
		}
	}
	return nil
}

func (f *componentSupervisorFixture) closeBorrowers() error {
	for _, endpoint := range f.endpoints {
		if endpoint.listener != nil {
			if err := endpoint.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				return err
			}
			<-endpoint.done
		}
	}
	for _, client := range f.clients {
		if err := client.Close(); err != nil {
			return err
		}
	}
	for _, td := range f.tasks {
		if err := td.Close(); err != nil {
			return err
		}
	}
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			return err
		}
	}
	if f.rootDirectory != nil {
		return f.rootDirectory.Close()
	}
	return nil
}

func (f *componentSupervisorFixture) checkPins(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.checkDirectory(); err != nil {
		return err
	}
	if f.store != nil {
		return checkComponentDirectory(f.rootDirectory, f.root)
	}
	return nil
}

func (f *componentSupervisorFixture) checkDirectory() error {
	return checkComponentDirectory(f.directory, f.base)
}

func checkComponentDirectory(directory *os.File, path string) error {
	if directory == nil {
		return errors.New("component directory pin is unavailable")
	}
	held, err := directory.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(held, current) {
		return errors.New("component directory identity changed")
	}
	return nil
}

func TestComponentSupervisorPortsStayExplicitAndRootBound(t *testing.T) {
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	base := pueue.Options{}
	configured := owner.setupOptions(base)
	if base.DaemonStartGuard != nil || base.PrivateGenerationGuard != nil || base.DaemonWaitOwner != nil || configured.DaemonStartGuard != owner || configured.PrivateGenerationGuard != owner || configured.DaemonWaitOwner != owner {
		t.Fatal("component ports became an implicit production grant")
	}
	callbacks := 0
	commit := func() error { callbacks++; return nil }
	if err := owner.GuardPrivateGenerationCommit(t.Context(), owner.root, commit); err != nil {
		t.Fatal(err)
	}
	if err := owner.GuardDaemonStart(t.Context(), owner.root+"-foreign", commit); !errors.Is(err, pueue.ErrInfrastructureOwnerRequired) {
		t.Fatal("component port accepted a foreign root")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := owner.GuardDaemonStart(canceled, owner.root, commit); !errors.Is(err, context.Canceled) {
		t.Fatal("component port ignored caller cancellation")
	}
	owner.mu.Lock()
	owner.closing = true
	owner.mu.Unlock()
	if err := owner.GuardDaemonStart(t.Context(), owner.root, commit); !errors.Is(err, pueue.ErrInfrastructureOwnerRequired) {
		t.Fatal("component port admitted work during cleanup")
	}
	if callbacks != 1 || len(owner.calls) != 1 || owner.calls[0].kind != "commit" {
		t.Fatal("component owner did not record the exact admitted callback")
	}
}

func TestComponentSupervisorSetupReturnsProductionDependenciesWithoutGrants(t *testing.T) {
	_, deps, _ := newSupervisorProbeTestFixture(t)
	options := deps.SupervisorOptions
	if options.DaemonStartGuard != nil || options.PrivateGenerationGuard != nil || options.DaemonWaitOwner != nil {
		t.Fatal("component setup leaked its explicit ports into production Dependencies")
	}
}

func TestComponentSupervisorCleanupRetainsBorrowersUntilWait(t *testing.T) {
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	store, err := taskdir.InitStore(owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	// This synthetic channel tests the completion port, not native placement.
	// Real component setup registers Pueue's sole daemon Wait channel.
	done := make(chan struct{})
	defer close(done)
	if err = owner.RegisterDaemonWait(owner.root, 123, done); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	if err = owner.close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished Wait did not retain fixture: %v", err)
	}
	if _, err = store.PinRootContext(t.Context()); err != nil {
		t.Fatal("unfinished Wait closed borrowed Store")
	}
	if _, err = os.Stat(owner.base); err != nil {
		t.Fatal("unfinished Wait removed fixture paths")
	}
}

func TestComponentSupervisorCleanupRefusesReplacedDirectory(t *testing.T) {
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	held := owner.base + "-held"
	renamed := false
	defer func() {
		if !renamed {
			return
		}
		if err := os.RemoveAll(owner.base); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(held, owner.base); err != nil {
			t.Error(err)
		}
	}()
	if err := os.Rename(owner.base, held); err != nil {
		t.Fatal(err)
	}
	renamed = true
	if err := os.Mkdir(owner.base, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(owner.base, "replacement-owned-by-test")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := owner.close(t.Context()); err == nil {
		t.Fatal("fixture cleanup accepted a replacement directory")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("fixture cleanup removed replacement contents")
	}
}
