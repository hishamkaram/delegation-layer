package infrastructure

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func shortEndpointRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "di-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			t.Error(removeErr)
		}
	})
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func endpointJournalAt(t *testing.T, root, temporary string) ownerJournalFixture {
	t.Helper()
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	journal, err := openIntentJournal(t.Context(), store, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, acquired, err := journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("election: %v %v", acquired, err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	intent := liveJournalIntent(t, journal)
	if temporary != "" {
		record := LaunchContext{Protocol: ProtocolVersion, Root: root, RootID: store.RootID, OS: runtime.GOOS, ControlEnvironment: []string{"TMPDIR=" + temporary}, MaintenanceEnvironment: []string{"TMPDIR=" + temporary}}
		reference, _, stageErr := journal.stageContext(t.Context(), record)
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		intent.Inputs.Context = reference
		refreshIntentDigest(t, &intent)
	}
	requireJournalPut(t, journal, lease, intent)
	assumption, publication := evidenceForIntent(t, intent)
	fixture := ownerJournalFixture{journal, lease, intent, assumption, publication}
	requireAssumptionPut(t, fixture)
	return fixture
}

func createEndpoint(t *testing.T, fixture ownerJournalFixture) *ownedEndpoint {
	t.Helper()
	endpoint, err := fixture.journal.listenEndpoint(t.Context(), fixture.lease, fixture.assumption.Startup)
	ownEndpointResult(t, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func ownEndpointResult(t *testing.T, endpoint *ownedEndpoint) {
	t.Helper()
	if endpoint != nil {
		t.Cleanup(func() {
			if closeErr := endpoint.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
}

func requireEndpointListenerClosed(t *testing.T, endpoint *ownedEndpoint) {
	t.Helper()
	// An incorrectly live listener must fail this assertion without blocking.
	if deadlineErr := endpoint.listener.SetDeadline(time.Now()); deadlineErr != nil && !errors.Is(deadlineErr, net.ErrClosed) {
		t.Fatal(deadlineErr)
	}
	if connection, err := endpoint.listener.AcceptUnix(); !errors.Is(err, net.ErrClosed) {
		if connection != nil {
			if closeErr := connection.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("listener closure not proven: %v", err)
	}
}

func TestEndpointBindsInitialPrivateRootSocket(t *testing.T) {
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	endpoint := createEndpoint(t, fixture)
	identity := endpoint.Identity()
	if identity.Path != filepath.Join(fixture.journal.pin.Root(), ".i") || identity.DirectoryDev != fixture.journal.pin.Device() || identity.DirectoryInode != fixture.journal.pin.Inode() {
		t.Fatal("endpoint did not retain actual root identity")
	}
	socket, err := observeEndpointSocket(identity.Path)
	if err != nil || !endpoint.sameSocket(socket) || uint32(socket.Mode)&0o7777 != 0o600 {
		t.Fatalf("private socket identity: %v", err)
	}
	checkEndpointCloexec(t, endpoint)
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: identity.Path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	accepted, err := endpoint.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	if err = accepted.Close(); err != nil {
		t.Fatal(err)
	}
	if err = endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	requireEndpointListenerClosed(t, endpoint)
	if _, err = os.Lstat(identity.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed endpoint residue: %v", err)
	}
}

func checkEndpointCloexec(t *testing.T, endpoint *ownedEndpoint) {
	t.Helper()
	raw, err := endpoint.listener.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var descriptorErr error
	if err = raw.Control(func(fd uintptr) { flags, descriptorErr = unix.FcntlInt(fd, unix.F_GETFD, 0) }); err != nil {
		t.Fatal(err)
	}
	if descriptorErr != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("listener descriptor is not close-on-exec: %v", descriptorErr)
	}
}

func TestEndpointRefusesOccupiedLiveAndDeadSockets(t *testing.T) {
	for _, live := range []bool{true, false} {
		name := "dead"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) { checkOccupiedEndpointSocket(t, live) })
	}
}

func checkOccupiedEndpointSocket(t *testing.T, live bool) {
	t.Helper()
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	path := filepath.Join(fixture.journal.pin.Root(), ".i")
	existing, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	existing.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		if live {
			if closeErr := existing.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if removeErr := os.Remove(path); removeErr != nil {
			t.Error(removeErr)
		}
	})
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		if err = existing.Close(); err != nil {
			t.Fatal(err)
		}
	}
	endpoint, listenErr := fixture.journal.listenEndpoint(t.Context(), fixture.lease, fixture.assumption.Startup)
	ownEndpointResult(t, endpoint)
	if endpoint != nil || !errors.Is(listenErr, os.ErrExist) {
		t.Fatalf("occupied socket accepted: %v", listenErr)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("occupied socket replaced: %v", err)
	}
}

func TestEndpointClosePreservesReplacementAndCanRetry(t *testing.T) {
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	endpoint := createEndpoint(t, fixture)
	path := endpoint.Identity().Path
	retained := path + "-retained"
	if err := os.Rename(path, retained); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.Remove(path); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			t.Error(cleanupErr)
		}
		if err := os.Rename(retained, path); err != nil {
			t.Error(err)
		}
		if err := endpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); !errors.Is(err, ErrIdentity) {
		t.Errorf("replacement cleanup was not refused: %v", err)
	}
	requireEndpointListenerClosed(t, endpoint)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement path was removed or changed: %v", err)
	}
}

func TestEndpointClosePreservesReplacementRootAndCanRetry(t *testing.T) {
	root := shortEndpointRoot(t)
	fixture := endpointJournalAt(t, root, "")
	endpoint := createEndpoint(t, fixture)
	metadata, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	retained := root + "-retained"
	if err = os.Rename(root, retained); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(root); cleanupErr != nil {
			t.Error(cleanupErr)
		}
		if cleanupErr := os.Rename(retained, root); cleanupErr != nil {
			t.Error(cleanupErr)
		}
		if closeErr := endpoint.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "root.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, ".i")
	if err = os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = endpoint.Close(); err == nil {
		t.Fatal("root replacement cleanup was accepted")
	}
	requireEndpointListenerClosed(t, endpoint)
	data, err := os.ReadFile(replacement)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement root socket path was changed: %v", err)
	}
}

func TestEndpointDoesNotDependOnOriginTemporaryDirectory(t *testing.T) {
	temporary := shortEndpointRoot(t)
	fixture := endpointJournalAt(t, shortEndpointRoot(t), temporary)
	if err := os.RemoveAll(temporary); err != nil {
		t.Fatal(err)
	}
	endpoint := createEndpoint(t, fixture)
	if endpoint.Identity().Path != filepath.Join(fixture.journal.pin.Root(), ".i") {
		t.Fatal("endpoint depended on origin temporary path")
	}
}

func TestEndpointNativeLengthErrorDoesNotRenderPath(t *testing.T) {
	parent := shortEndpointRoot(t)
	root := filepath.Join(parent, strings.Repeat("private-context-", 12))
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := endpointJournalAt(t, root, "")
	endpoint, err := fixture.journal.listenEndpoint(t.Context(), fixture.lease, fixture.assumption.Startup)
	ownEndpointResult(t, endpoint)
	if endpoint != nil || err == nil {
		t.Fatal("overlong native endpoint was accepted")
	}
	var refusal *endpointError
	if !errors.As(err, &refusal) || refusal.PathBytes() != len(filepath.Join(root, ".i")) {
		t.Fatalf("typed endpoint length lost: %v", err)
	}
	var native *net.OpError
	if !errors.As(err, &native) || native.Err == nil {
		t.Fatal("native bind cause was not preserved")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "private-context") {
		t.Fatal("endpoint error rendered private pathname")
	}
}

func TestEndpointRequiresStoredSelfAssumption(t *testing.T) {
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	changed := fixture.assumption
	changed.UID++
	data, err := task.MarshalCanonical(changed)
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(fixture.journal.pin.Root(), "infrastructure", assumptionName(changed.Startup.Sequence))
	if err = os.WriteFile(record, data, 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, listenErr := fixture.journal.listenEndpoint(t.Context(), fixture.lease, changed.Startup)
	ownEndpointResult(t, endpoint)
	if endpoint != nil || !errors.Is(listenErr, ErrIdentity) {
		t.Fatalf("non-self stored assumption accepted: %v", listenErr)
	}
	if _, err = os.Lstat(filepath.Join(fixture.journal.pin.Root(), ".i")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-self refusal created socket: %v", err)
	}
}

func TestEndpointRefusesOccupiedNonSocketPaths(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) { checkOccupiedEndpointPath(t, kind) })
	}
}

func checkOccupiedEndpointPath(t *testing.T, kind string) {
	t.Helper()
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	path := filepath.Join(fixture.journal.pin.Root(), ".i")
	switch kind {
	case "file":
		if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "symlink":
		if err := os.Symlink("root.json", path); err != nil {
			t.Fatal(err)
		}
	case "directory":
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, listenErr := fixture.journal.listenEndpoint(t.Context(), fixture.lease, fixture.assumption.Startup)
	ownEndpointResult(t, endpoint)
	if endpoint != nil || !errors.Is(listenErr, os.ErrExist) {
		t.Fatalf("occupied %s accepted: %v", kind, listenErr)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("occupied %s replaced: %v", kind, err)
	}
}

func TestEndpointCancelledCleanupClosesListenerAndRetainsRetry(t *testing.T) {
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	endpoint := createEndpoint(t, fixture)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := endpoint.closeContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup did not report incomplete removal: %v", err)
	}
	requireEndpointListenerClosed(t, endpoint)
	socket, err := observeEndpointSocket(endpoint.Identity().Path)
	if err != nil || !endpoint.sameSocket(socket) {
		t.Fatalf("canceled cleanup removed/lost owned socket: %v", err)
	}
	if err = endpoint.Close(); err != nil {
		t.Fatalf("fresh bounded cleanup retry: %v", err)
	}
	if _, err = os.Lstat(endpoint.Identity().Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup retry left socket residue: %v", err)
	}
}

type rootSwapEndpointObserver struct {
	store          *taskdir.Store
	moved          bool
	triggered      bool
	originalSocket unix.Stat_t
}

func (o *rootSwapEndpointObserver) PinRootContext(ctx context.Context) (taskdir.RootPin, error) {
	if !o.triggered {
		o.triggered = true
		socket, err := observeEndpointSocket(filepath.Join(o.store.Root, ".i"))
		if err != nil {
			return taskdir.RootPin{}, err
		}
		o.originalSocket = socket
		metadata, err := os.ReadFile(filepath.Join(o.store.Root, "root.json"))
		if err != nil {
			return taskdir.RootPin{}, err
		}
		if err = os.Rename(o.store.Root, o.store.Root+"-retained"); err != nil {
			return taskdir.RootPin{}, err
		}
		o.moved = true
		if err = os.Mkdir(o.store.Root, 0o700); err != nil {
			return taskdir.RootPin{}, err
		}
		if err = os.WriteFile(filepath.Join(o.store.Root, "root.json"), metadata, 0o600); err != nil {
			return taskdir.RootPin{}, err
		}
		if err = os.WriteFile(filepath.Join(o.store.Root, ".i"), []byte("replacement"), 0o600); err != nil {
			return taskdir.RootPin{}, err
		}
	}
	// Every observation, including the injected failure, uses the real native
	// Store pin. No lease, boot, self identity or pin check is substituted.
	return o.store.PinRootContext(ctx)
}

func restoreEndpointRoot(t *testing.T, observer *rootSwapEndpointObserver) {
	t.Helper()
	if !observer.moved {
		return
	}
	if err := os.RemoveAll(observer.store.Root); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(observer.store.Root+"-retained", observer.store.Root); err != nil {
		t.Error(err)
		return
	}
	observer.moved = false
}

func TestEndpointFailedBirthRetainsCleanupOwnership(t *testing.T) {
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	observer := &rootSwapEndpointObserver{store: fixture.journal.store}
	endpoint, err := fixture.journal.listenEndpointWithObserver(t.Context(), fixture.lease, fixture.assumption.Startup, observer)
	// Own every returned resource before interpreting the error. Restore runs
	// before cleanup retry, and both precede borrowed journal/store/root teardown.
	ownEndpointResult(t, endpoint)
	t.Cleanup(func() { restoreEndpointRoot(t, observer) })
	if err == nil || endpoint == nil {
		t.Fatalf("failed birth discarded unresolved cleanup ownership: handle=%v error=%v", endpoint != nil, err)
	}
	requireEndpointListenerClosed(t, endpoint)
	identity := endpoint.Identity()
	if identity.SocketInode == 0 || identity.SocketInode != uint64(observer.originalSocket.Ino) || identity.SocketDev != uint64(observer.originalSocket.Dev) {
		t.Fatal("failed birth lost original native socket identity")
	}
	data, readErr := os.ReadFile(filepath.Join(fixture.journal.pin.Root(), ".i"))
	if readErr != nil || string(data) != "replacement" {
		t.Fatalf("failed birth cleanup changed replacement: %v", readErr)
	}
	restoreEndpointRoot(t, observer)
	if closeErr := endpoint.Close(); closeErr != nil {
		t.Fatalf("restored original root could not finish retained cleanup: %v", closeErr)
	}
	if _, statErr := os.Lstat(identity.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("retry left original socket residue: %v", statErr)
	}
}

func TestEndpointCleanupUsesIdentityAfterSocketModeChange(t *testing.T) {
	fixture := endpointJournalAt(t, shortEndpointRoot(t), "")
	endpoint := createEndpoint(t, fixture)
	identity := endpoint.Identity()
	if err := os.Chmod(identity.Path, 0o400); err != nil {
		t.Fatal(err)
	}
	socket, err := observeEndpointSocket(identity.Path)
	if err != nil || !endpoint.sameSocket(socket) || uint32(socket.Mode)&0o7777 != 0o400 {
		t.Fatalf("fixture did not change mode on original socket: %v", err)
	}
	if err = endpoint.Close(); err != nil {
		t.Fatalf("mode change prevented identity-checked cleanup: %v", err)
	}
	requireEndpointListenerClosed(t, endpoint)
	if _, err = os.Lstat(identity.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup left owned socket: %v", err)
	}
}
