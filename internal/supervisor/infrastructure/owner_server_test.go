package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The same-process component fixture skips managed-image/native-birth admission.
// It uses canonical journals, actual self observations and native Unix peers;
// it proves serving ownership, not a separately established resident/product.
type ownerServerFixture struct {
	root                 string
	entry                *ownedResidentEntry
	server               *ownerServer
	served               chan error
	retain               bool
	expectedCleanupFault error
}

type ownerServerTestHost struct {
	componentEntryHost
	ensure       func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error)
	continuation func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	recoverSaved func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	manual       func(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, ManualMigrationReport, error)
	classify     func(error) PreparationFailure
}

func (h *ownerServerTestHost) EnsureOwnRoot(ctx context.Context, operation Operation, selected LaunchContext, guard ResidentGuards, waits DaemonWaitOwner) (Disposition, error) {
	if h.ensure == nil {
		return Disposition{Kind: DispositionWarmReuse}, nil
	}
	return h.ensure(ctx, operation, selected, guard, waits)
}

func (h *ownerServerTestHost) RecoverSaved(ctx context.Context, operation Operation, selected LaunchContext, source SavedBindingSource, guards ResidentGuards, waits DaemonWaitOwner) (Disposition, error) {
	if h.recoverSaved == nil {
		return h.componentEntryHost.RecoverSaved(ctx, operation, selected, source, guards, waits)
	}
	return h.recoverSaved(ctx, operation, selected, source, guards, waits)
}

func (h *ownerServerTestHost) PrepareContinuation(ctx context.Context, operation Operation, selected LaunchContext, source SavedBindingSource, guards ResidentGuards, waits DaemonWaitOwner) (Disposition, error) {
	if h.continuation == nil {
		return h.componentEntryHost.PrepareContinuation(ctx, operation, selected, source, guards, waits)
	}
	return h.continuation(ctx, operation, selected, source, guards, waits)
}

func (h *ownerServerTestHost) MigrateOwnRoot(ctx context.Context, op Operation, selected LaunchContext, guards ResidentGuards, waits DaemonWaitOwner) (Disposition, ManualMigrationReport, error) {
	if h.manual != nil {
		return h.manual(ctx, op, selected, guards, waits)
	}
	return h.componentEntryHost.MigrateOwnRoot(ctx, op, selected, guards, waits)
}

func (h *ownerServerTestHost) ClassifyPreparation(err error) PreparationFailure {
	if h.classify != nil {
		return h.classify(err)
	}
	return h.componentEntryHost.ClassifyPreparation(err)
}

type ownerServerFixtureSelection struct {
	initial   InitialOperationKind
	deadline  time.Time
	artifacts Artifacts
}

func newOwnerServerFixture(t *testing.T, host *ownerServerTestHost) *ownerServerFixture {
	t.Helper()
	return newOwnerServerFixtureWithSelection(t, host, ownerServerFixtureSelection{})
}

func newOwnerServerFixtureWithSelection(t *testing.T, host *ownerServerTestHost, selection ownerServerFixtureSelection) *ownerServerFixture {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".os-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ownerServerFixture{root: root, entry: &ownedResidentEntry{}}
	t.Cleanup(func() { fixture.cleanup(t) })
	entry := fixture.entry
	entry.store, err = taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry.journal, err = openIntentJournal(t.Context(), entry.store, true)
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	entry.lease, acquired, err = entry.journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("fixture lease: acquired=%v err=%v", acquired, err)
	}
	selected := contextForJournal(t, entry.journal)
	selected.OS = runtime.GOOS
	selected.ControlEnvironment = append(selected.ControlEnvironment, "OWNER_SERVER_FIXTURE_CONTEXT=selected")
	reference, _, err := entry.journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	intent := ownerServerIntentFixture(t, entry.journal, reference, selection)
	requireJournalPut(t, entry.journal, entry.lease, intent)
	entry.operation, err = newRootOwnerOperation(intent, selected)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err = entry.assumeAndPublish(t.Context(), startup); err != nil {
		t.Fatal(err)
	}
	fixture.server, err = newOwnerServer(t.Context(), entry, host)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *ownerServerFixture) birthContext(t *testing.T) LaunchContext {
	t.Helper()
	selected, err := f.entry.journal.readContext(t.Context(), f.entry.operation.defaultContextReference())
	if err != nil {
		t.Fatalf("read fixture birth context: %v", err)
	}
	return selected
}

func ownerServerIntentFixture(t *testing.T, journal *intentJournal, reference LaunchContextReference, selection ownerServerFixtureSelection) StartupIntent {
	t.Helper()
	intent := liveJournalIntent(t, journal)
	intent.Inputs.Initial = ownerInitialOperationFixture(journal.pin.RootID(), selection.initial)
	if selection.artifacts != (Artifacts{}) {
		intent.Inputs.Artifacts = selection.artifacts
	}
	if intent.Inputs.Initial != (InitialOperation{}) {
		intent.Protocol = RecordProtocolVersion
	}
	if !selection.deadline.IsZero() {
		intent.Deadline = selection.deadline.UTC().Format(time.RFC3339Nano)
	}
	intent.Inputs.Context = reference
	refreshIntentDigest(t, &intent)
	return intent
}

func (f *ownerServerFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if f.server != nil {
		err := f.server.stopAcceptAndDrain(ctx)
		select {
		case <-f.server.drained:
			if err != nil && !errors.Is(err, f.expectedCleanupFault) {
				t.Errorf("retained server cleanup fault at %s: %v", f.root, err)
				return
			}
			if diagnostic := f.server.diagnostic(); diagnostic != nil {
				t.Logf("bounded request diagnostic: %v", diagnostic)
			}
		default:
			t.Errorf("retained undrained server fixture %s: %v", f.root, err)
			return
		}
	}
	if f.served != nil {
		select {
		case err := <-f.served:
			if err != nil {
				t.Logf("serving diagnostic: %v", err)
			}
		case <-ctx.Done():
			t.Errorf("retained serving fixture %s: %v", f.root, ctx.Err())
			return
		}
	}
	if f.retain {
		t.Errorf("retained unresolved server fixture %s", f.root)
		return
	}
	if err := f.entry.closeContext(ctx); err != nil {
		t.Errorf("retained resident fixture %s: %v", f.root, err)
		return
	}
	if err := os.RemoveAll(f.root); err != nil {
		t.Error(err)
	}
}

func (f *ownerServerFixture) start() { f.startContext(context.Background()) }

func (f *ownerServerFixture) startContext(ctx context.Context) {
	f.served = make(chan error, 1)
	go func() { f.served <- f.server.serve(ctx) }()
}

func (f *ownerServerFixture) request(t *testing.T, action ownerAction) ownerRequest {
	t.Helper()
	return f.requestContext(t, t.Context(), action)
}

func (f *ownerServerFixture) requestContext(t *testing.T, ctx context.Context, action ownerAction) ownerRequest {
	t.Helper()
	intent, assumption, publication, _, err := f.server.readResident(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := publication.Digest(intent, assumption)
	if err != nil {
		t.Fatal(err)
	}
	return ownerRequest{Protocol: ownerProtocolVersion, RootID: intent.Inputs.RootID, Owner: assumption.Startup, PublicationSHA256: digest, Client: assumption.Process, Action: action, Deadline: time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339Nano)}
}

func (f *ownerServerFixture) dial(t *testing.T) *net.UnixConn {
	t.Helper()
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: f.entry.endpoint.Identity().Path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
	})
	if err = connection.SetDeadline(time.Now().Add(12 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return connection
}

func readServerReply(t *testing.T, connection *net.UnixConn) ownerReply {
	t.Helper()
	reply, err := readOwnerFrame[ownerReply](bufio.NewReader(connection))
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestOwnerServerStatusAndEnsureUseActualPublicationAndSelectedContext(t *testing.T) {
	var calls atomic.Int32
	host := &ownerServerTestHost{ensure: func(_ context.Context, _ Operation, selected LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		if selected.ControlEnvironment[0] != "PATH=/selected/bin" {
			t.Error("accepted selected context was not resolved")
		}
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}
	f := newOwnerServerFixture(t, host)
	selected := contextForJournal(t, f.entry.journal)
	selected.OS = runtime.GOOS
	selected.ControlEnvironment = []string{"PATH=/selected/bin"}
	reference, _, err := f.entry.journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	f.start()
	for _, action := range []ownerAction{ownerStatus, ownerEnsureOwn} {
		request := f.request(t, action)
		if action == ownerEnsureOwn {
			request.Context = reference
		}
		connection := f.dial(t)
		if err = writeOwnerFrame(connection, request); err != nil {
			t.Fatal(err)
		}
		reply := readServerReply(t, connection)
		intent, assumption, publication, _, err := f.server.readResident(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err = validateOwnerReplyBinding(intent, assumption, publication, request, reply); err != nil {
			t.Fatal(err)
		}
		if reply.Code != ownerOK || reply.Context != request.Context {
			t.Fatalf("wrong operation reply: %+v", reply)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("host calls=%d", calls.Load())
	}
}

func TestOwnerServerDisconnectDoesNotCancelAcceptedWorkAndDrainPreservesRegistration(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	host := &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, waits DaemonWaitOwner) (Disposition, error) {
		entered <- ctx
		<-release
		done := make(chan struct{})
		close(done)
		if err := waits.RegisterDaemonWait("", 1, done); !errors.Is(err, ErrIdentity) {
			t.Errorf("foreign root wait accepted: %v", err)
		}
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}
	f := newOwnerServerFixture(t, host)
	t.Cleanup(unblock)
	serveContext, stopServing := context.WithCancel(t.Context())
	defer stopServing()
	f.startContext(serveContext)
	request := f.request(t, ownerEnsureOwn)
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	ctx := awaitOwnerServerHost(t, entered)
	deadline, finite := ctx.Deadline()
	expected, err := canonicalStartupTime(request.Deadline)
	if err != nil || !finite || !deadline.Equal(expected) {
		t.Fatalf("accepted deadline changed: %v %v", deadline, err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	stopServing()
	f.server.stopAccepting()
	if err := ctx.Err(); err != nil {
		t.Fatalf("disconnect/stop canceled accepted work: %v", err)
	}
	select {
	case <-f.server.drained:
		t.Fatal("drain completed while host owns work")
	default:
	}
	done := make(chan struct{})
	close(done)
	if err := f.server.waits.RegisterDaemonWait(f.root, 1, done); err != nil {
		t.Fatalf("Wait registration closed before host drain: %v", err)
	}
	unblock()
	drainCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.server.stopAcceptAndDrain(drainCtx); err != nil {
		t.Fatalf("ordinary disconnect poisoned clean drain: %v", err)
	}
	if f.server.diagnostic() == nil {
		t.Fatal("disconnect diagnostic was lost")
	}
	select {
	case <-f.server.drained:
	default:
		t.Fatal("handlers not drained")
	}
	// Callback drain does not join the canonical worker retained by the host.
	// Resident cleanup owns the later registration boundary.
	if err := f.server.waits.RegisterDaemonWait(f.root, 1, done); err != nil {
		t.Fatalf("callback drain closed registration before resident cleanup: %v", err)
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("transport fault closed resident: %v", err)
	}
}

func awaitOwnerServerHost(t *testing.T, entered <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-entered:
		return ctx
	case <-time.After(10 * time.Second):
		t.Fatal("host did not receive authenticated work")
		return nil
	}
}

func TestOwnerServerFixtureSelectsNativeContext(t *testing.T) {
	fixture := newOwnerServerFixture(t, &ownerServerTestHost{})
	operation := fixture.entry.operation
	selected := fixture.birthContext(t)
	if selected.OS != runtime.GOOS {
		t.Fatal("resident fixture selected a foreign operating system")
	}
	selected.ControlEnvironment = append(selected.ControlEnvironment, "OWNER_SERVER_FIXTURE_REHASH=positive")
	digest, err := selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest == operation.Intent().Inputs.Context.SHA256 {
		t.Fatal("positive control did not change context bytes")
	}
	intent := operation.Intent()
	intent.Inputs.Context = LaunchContextReference{SHA256: digest}
	refreshIntentDigest(t, &intent)
	positive, err := newRootOwnerOperation(intent, selected)
	if err != nil || positive.Intent() != intent || positive.Artifacts() != operation.Artifacts() {
		t.Fatalf("native context rehash failed or changed root/source identity: %v", err)
	}
	foreign := selected.Clone()
	if runtime.GOOS == "linux" {
		foreign.OS = "darwin"
	} else {
		foreign.OS = "linux"
	}
	digest, err = foreign.Digest()
	if err != nil {
		t.Fatal(err)
	}
	foreignIntent := intent
	foreignIntent.Inputs.Context = LaunchContextReference{SHA256: digest}
	refreshIntentDigest(t, &foreignIntent)
	if _, err = newRootOwnerOperation(foreignIntent, foreign); !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign context did not refuse at native identity boundary: %v", err)
	}
}
