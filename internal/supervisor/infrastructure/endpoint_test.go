package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
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
		record := LaunchContext{Protocol: RecordProtocolVersion, Root: root, RootID: store.RootID, OS: runtime.GOOS, ControlEnvironment: []string{"TMPDIR=" + temporary}, MaintenanceEnvironment: []string{"TMPDIR=" + temporary}, SupervisorExecutable: filepath.Join(root, "pueue")}
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
	if identity.Path != ownerEndpointPath(fixture.journal.pin.Root(), fixture.intent.Sequence) || identity.DirectoryDev != fixture.journal.pin.Device() || identity.DirectoryInode != fixture.journal.pin.Inode() {
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
	path := ownerEndpointPath(fixture.journal.pin.Root(), fixture.intent.Sequence)
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
	replacement := ownerEndpointPath(root, fixture.intent.Sequence)
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
	if endpoint.Identity().Path != ownerEndpointPath(fixture.journal.pin.Root(), fixture.intent.Sequence) {
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
	if !errors.As(err, &refusal) || refusal.pathBytes != len(ownerEndpointPath(root, fixture.intent.Sequence)) {
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
	if _, err = os.Lstat(ownerEndpointPath(fixture.journal.pin.Root(), fixture.intent.Sequence)); !errors.Is(err, os.ErrNotExist) {
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
	path := ownerEndpointPath(fixture.journal.pin.Root(), fixture.intent.Sequence)
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
	sequence       uint64
	moved          bool
	triggered      bool
	originalSocket unix.Stat_t
}

func (o *rootSwapEndpointObserver) PinRootContext(ctx context.Context) (taskdir.RootPin, error) {
	if !o.triggered {
		o.triggered = true
		socket, err := observeEndpointSocket(ownerEndpointPath(o.store.Root, o.sequence))
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
		if err = os.WriteFile(ownerEndpointPath(o.store.Root, o.sequence), []byte("replacement"), 0o600); err != nil {
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
	observer := &rootSwapEndpointObserver{store: fixture.journal.store, sequence: fixture.intent.Sequence}
	endpoint, err := bindInitialEndpoint(t.Context(), observer, fixture.journal.pin, fixture.intent)
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
	data, readErr := os.ReadFile(ownerEndpointPath(fixture.journal.pin.Root(), fixture.intent.Sequence))
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

func TestOwnerEndpointCanonicalSequencePaths(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "state")
	for _, tc := range []struct {
		sequence uint64
		leaf     string
	}{
		{1, "0000000000001"}, {2, "0000000000002"}, {10, "000000000000a"}, {417, "00000000000bl"}, {0x0102030405060708, "00jv30clup820"}, {^uint64(0), "3w5e11264sgsf"},
	} {
		want := filepath.Join(root, tc.leaf)
		if got := ownerEndpointPath(root, tc.sequence); got != want {
			t.Fatalf("sequence %d path = %q, want %q", tc.sequence, got, want)
		}
		if len(tc.leaf) != 13 || OwnerEndpointPathBytes(root) != len(want) {
			t.Fatal("endpoint width changed")
		}
	}
	unicodeRoot := filepath.Join(root, "家", "δοκιμή")
	want := filepath.Join(unicodeRoot, "0000000000001")
	if ownerEndpointPath(unicodeRoot, 1) != want || OwnerEndpointPathBytes(unicodeRoot) != len(want) || len(want) != len(unicodeRoot)+14 {
		t.Fatal("multibyte root byte budget changed")
	}
	if OwnerEndpointPathBytes(root) != len(ownerEndpointPath(root, ^uint64(0))) || OwnerEndpointPathBytes(unicodeRoot) != len(ownerEndpointPath(unicodeRoot, ^uint64(0))) {
		t.Fatal("preflight byte budget does not cover maximum sequence")
	}
}

// These probes exercise real inherited leases, native socket birth and process
// exit using the Go-test-only host stub. They prove neither managed CLI image
// eligibility nor the startup settlement owner's complete successor predicate.
func TestSequenceEndpointCrashChild(t *testing.T) {
	mode, invocation, selected := componentChildInvocation(t)
	if !selected {
		return
	}
	if mode == "before-publication" {
		bindSequenceCrashBeforePublication(t, invocation)
		os.Exit(0) // Deliberately bypass cleanup to leave the bound socket residue.
	}
	entry, err := consumeOwnerInvocation(t.Context(), invocation, &componentEntryHost{})
	ownComponentEntry(t, entry)
	if err != nil || entry == nil || entry.grant == nil {
		t.Fatalf("second native birth failed: %v", err)
	}
	if mode == "after-publication" {
		os.Exit(0)
	} // Kernel closes actor FDs; path remains.
	if mode != "successor" {
		t.Fatal("unknown sequence component mode")
	}
	ready := os.NewFile(4, "sequence-component-ready")
	if _, err = ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err = ready.Close(); err != nil {
		t.Fatal(err)
	}
	serveSequenceEndpointContact(t, entry, invocation.startup)
}

func bindSequenceCrashBeforePublication(t *testing.T, invocation ownerInvocation) {
	t.Helper()
	entry := &ownedResidentEntry{}
	ownComponentEntry(t, entry)
	if err := matchOwnerInheritedDescriptor(invocation.root); err != nil {
		t.Fatal(err)
	}
	var err error
	entry.lease, err = AdoptInheritedLease(os.NewFile(3, "sequence-component-lease"), filepath.Join(invocation.root, instanceLeaseName))
	if err != nil {
		t.Fatal(err)
	}
	if err = entry.prepareRoot(t.Context(), invocation, &componentEntryHost{}); err != nil {
		t.Fatal(err)
	}
	if _, err = entry.assumeAndBind(t.Context(), invocation.startup); err != nil {
		t.Fatal(err)
	}
	if entry.grant != nil {
		t.Fatal("pre-publication birth minted a grant")
	}
}

func serveSequenceEndpointContact(t *testing.T, entry *ownedResidentEntry, reference StartupReference) {
	t.Helper()
	if err := entry.endpoint.listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, err := entry.endpoint.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	request, err := readOwnerFrame[ownerRequest](bufio.NewReader(connection))
	if err != nil {
		t.Fatal(err)
	}
	assumption, err := entry.journal.readAssumption(t.Context(), reference)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := entry.journal.readPublication(t.Context(), reference)
	if err != nil {
		t.Fatal(err)
	}
	if err = authenticateOwnerRequest(t.Context(), connection, entry.operation.Intent(), assumption, publication, request); err != nil {
		t.Fatal(err)
	}
	if request.Action != ownerStatus {
		t.Fatal("component only serves status")
	}
	reply := ownerReply{Protocol: ownerProtocolVersion, RootID: request.RootID, OwnerRef: reference, PublicationSHA256: request.PublicationSHA256, Action: ownerStatus, Code: ownerOK}
	if err = writeOwnerFrame(connection, reply); err != nil {
		t.Fatal(err)
	}
	// Retain the native endpoint until the client completes its final recheck.
	if err = awaitOwnerContactEOF(connection); err != nil {
		t.Fatal(err)
	}
}

type sequenceEndpointChild struct {
	fixture *componentEntryFixture
	command *exec.Cmd
	output  bytes.Buffer
	ready   *os.File
	done    chan error
	joined  bool
	result  error
}

func launchSequenceEndpointChild(t *testing.T, fixture *componentEntryFixture, mode string) *sequenceEndpointChild {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	child := &sequenceEndpointChild{fixture: fixture, ready: read}
	t.Cleanup(func() { child.close(t) })
	defer func() {
		if closeErr := write.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	args := []string{"-test.run=^TestSequenceEndpointCrashChild$", "-test.timeout=15s", "--", "--owner-entry-component-test", mode}
	args = append(args, componentArguments(t, fixture)...)
	child.command = exec.Command(binary, args...)
	child.command.Stdout, child.command.Stderr = &child.output, &child.output
	err = fixture.lease.WithHeldFile(t.Context(), func(file *os.File) error {
		child.command.ExtraFiles = []*os.File{file, write}
		return child.command.Start()
	})
	if err != nil {
		t.Fatal(err)
	}
	child.done = make(chan error, 1)
	go func() { child.done <- child.command.Wait() }()
	// Closing this descriptor never LOCK_UNs the child's inherited flock.
	if err = fixture.lease.Close(); err != nil {
		fixture.retain = true
		t.Fatal(err)
	}
	fixture.lease = nil
	return child
}

func (c *sequenceEndpointChild) join(t *testing.T) {
	t.Helper()
	if c.done == nil || c.joined {
		return
	}
	select {
	case c.result = <-c.done:
		c.joined = true
	case <-time.After(20 * time.Second):
		c.fixture.retain = true
		t.Fatalf("retain component PID %d: exact child Wait unresolved", c.command.Process.Pid)
	}
	if c.result != nil {
		t.Fatalf("sequence component failed: %v\n%s", c.result, c.output.String())
	}
}

func (c *sequenceEndpointChild) close(t *testing.T) {
	t.Helper()
	if c.ready != nil {
		if err := c.ready.Close(); err != nil {
			c.fixture.retain = true
			t.Error(err)
		}
		c.ready = nil
	}
	c.join(t)
}

func (c *sequenceEndpointChild) awaitPublication(t *testing.T) {
	t.Helper()
	if err := c.ready.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(c.ready, ready[:]); err != nil {
		t.Fatalf("second native birth publication missing: %v", err)
	}
	if ready[0] != 1 {
		t.Fatal("invalid component publication marker")
	}
}

func TestSequenceEndpointSurvivesCrashResidue(t *testing.T) {
	for _, phase := range []string{"before-publication", "after-publication"} {
		t.Run(phase, func(t *testing.T) { exerciseSequenceEndpointCrash(t, phase) })
	}
}

func exerciseSequenceEndpointCrash(t *testing.T, phase string) {
	t.Helper()
	fixture := newComponentEntryFixture(t, "valid")
	crashed := launchSequenceEndpointChild(t, fixture, phase)
	crashed.join(t) // Exact parent cmd.Wait before interpreting PID or lease absence.
	oldPath := ownerEndpointPath(fixture.root, fixture.intent.Sequence)
	oldSocket, err := observeEndpointSocket(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	requireSequenceCrashClosed(t, fixture, crashed.command.Process.Pid, phase)
	appendSequenceSuccessor(t, fixture)
	successor := launchSequenceEndpointChild(t, fixture, "successor")
	successor.awaitPublication(t)
	contactSequenceSuccessor(t, fixture)
	successor.join(t)
	current, err := observeEndpointSocket(oldPath)
	if err != nil || current.Dev != oldSocket.Dev || current.Ino != oldSocket.Ino || current.Mode != oldSocket.Mode || current.Uid != oldSocket.Uid {
		t.Fatalf("old socket identity changed: %v", err)
	}
	if _, err = os.Lstat(ownerEndpointPath(fixture.root, fixture.intent.Sequence)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed successor left endpoint residue: %v", err)
	}
	requireSequenceInstanceFree(t, fixture)
}

func requireSequenceCrashClosed(t *testing.T, fixture *componentEntryFixture, pid int, phase string) {
	t.Helper()
	assumption, err := fixture.journal.readAssumption(t.Context(), fixture.reference)
	if err != nil || assumption.Process.PID != pid {
		t.Fatalf("crashed child assumption: %v", err)
	}
	reference, err := assumption.Process.Reference()
	if err != nil {
		t.Fatal(err)
	}
	present, err := lifetime.ProcessPresent(reference)
	if err != nil || present {
		fixture.retain = true
		t.Fatalf("crashed native child remains present: %v", err)
	}
	_, err = fixture.journal.readPublication(t.Context(), fixture.reference)
	if phase == "before-publication" {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pre-publication crash produced publication or unreadable evidence: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	requireSequenceInstanceFree(t, fixture)
}

func requireSequenceInstanceFree(t *testing.T, fixture *componentEntryFixture) {
	t.Helper()
	lease, acquired, err := fixture.journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		fixture.retain = true
		t.Fatalf("native instance lease remains held: %v", err)
	}
	fixture.lease = lease
}

// Dense intent-only rows represent canonical historical fixture data. They are
// not settlement evidence or a claim that the startup settlement owner may skip its
// complete successor predicate. The actual old child was already joined/absent.
func appendSequenceSuccessor(t *testing.T, fixture *componentEntryFixture) {
	t.Helper()
	// Durable dense fixture setup has an explicit finite caller budget instead
	// of taskdir's short fallback for callers with no deadline. No latency claim.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	err := fixture.journal.withLeaseTransaction(ctx, fixture.lease, func(tx *taskdir.ControlTransaction) error {
		for sequence := uint64(2); sequence < 417; sequence++ {
			historical := fixture.intent
			historical.Sequence, historical.Epoch = sequence, fmt.Sprintf("%032x", sequence)
			if _, err := historical.Digest(); err != nil {
				return err
			}
			created, err := tx.Put(intentName(sequence), historical)
			if err != nil {
				return err
			}
			if !created {
				return errors.New("historical fixture row already exists")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	next := fixture.intent
	next.Sequence = 417
	next.Epoch = strings.Repeat("b", 32)
	next.AcceptedAt = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	next.Deadline = time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	refreshIntentDigest(t, &next)
	created, err := fixture.journal.putIntent(ctx, fixture.lease, next)
	if err != nil || !created {
		t.Fatalf("append fixture successor: created=%t err=%v", created, err)
	}
	fixture.intent = next
	fixture.reference, err = ReferenceStartup(next)
	if err != nil {
		t.Fatal(err)
	}
}

func contactSequenceSuccessor(t *testing.T, fixture *componentEntryFixture) {
	t.Helper()
	publication, err := fixture.journal.readPublication(t.Context(), fixture.reference)
	if err != nil {
		t.Fatal(err)
	}
	if publication.Endpoint.Path != filepath.Join(fixture.root, "00000000000bl") {
		t.Fatal("second sequence publication path differs")
	}
	digest, err := publication.Digest(fixture.intent, mustReadSequenceAssumption(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	request := ownerRequest{Protocol: ownerProtocolVersion, RootID: fixture.store.RootID, Owner: fixture.reference, PublicationSHA256: digest, Client: ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()}, Action: ownerStatus, Deadline: time.Now().UTC().Add(5 * time.Second).Format(time.RFC3339Nano)}
	reply, err := contactOwner(t.Context(), contextPassiveReader(t, fixture.journal), fixture.journal.pin, request)
	if err != nil || reply.Code != ownerOK {
		t.Fatalf("new native endpoint authentication failed: %v", err)
	}
}

func mustReadSequenceAssumption(t *testing.T, fixture *componentEntryFixture) StartupAssumption {
	t.Helper()
	assumption, err := fixture.journal.readAssumption(t.Context(), fixture.reference)
	if err != nil {
		t.Fatal(err)
	}
	return assumption
}

func TestOwnerEndpointCaseFoldInjectivity(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "state")
	seen := make(map[string]uint64)
	for _, sequence := range []uint64{1, 417, ^uint64(0)} {
		name := filepath.Base(ownerEndpointPath(root, sequence))
		folded := strings.ToLower(name)
		if len(name) != 13 || name != folded || strings.Trim(name, "0123456789abcdefghijklmnopqrstuvwxyz") != "" {
			t.Fatalf("noncanonical reserved leaf %q", name)
		}
		if previous, exists := seen[folded]; exists {
			t.Fatalf("casefold aliases sequences %d and %d", previous, sequence)
		}
		seen[folded] = sequence
	}
}

func TestOwnerEndpointNamespaceExcludesFixedRootNames(t *testing.T) {
	// Fixed/lazy root entries from taskdir store/control/workspace/infrastructure,
	// scheduler host, and the private-supervisor layout. This is a namespace
	// collision assertion, never an inventory or permission to delete by shape.
	for _, name := range []string{
		"root.json", ".probe", ".maintenance.lock", ".root-admission.lock", instanceLeaseName, ".scheduler-host.lock", ".supervisor",
		"tasks", "inspections", "inspection-group", "runner-repair", "workspaces", "workspace-registry", "workspace-operations", "infrastructure", "scheduler-host",
	} {
		if len(name) == 13 && strings.Trim(name, "0123456789abcdefghijklmnopqrstuvwxyz") == "" {
			t.Fatalf("fixed root name enters reserved endpoint namespace: %s", name)
		}
	}
}
