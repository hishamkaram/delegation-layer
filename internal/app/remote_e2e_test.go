package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
)

func (e *remoteServerExchange) exchange(ctx context.Context, request remote.Request) (remote.Response, error) {
	var input bytes.Buffer
	framer := remote.Framer{Reader: &input, Writer: &input}
	if err := framer.WriteRequest(request); err != nil {
		return remote.Response{}, err
	}
	received, err := framer.ReadRequest()
	if err != nil {
		return remote.Response{}, err
	}
	response := executeRemote(ctx, received, e.deps)
	e.calls++
	if !e.lost && request.Operation == e.loseOperation {
		e.lost = true
		return remote.Response{}, io.ErrUnexpectedEOF
	}
	var output bytes.Buffer
	framer = remote.Framer{Reader: &output, Writer: &output}
	if err = framer.WriteResponse(response); err != nil {
		return remote.Response{}, err
	}
	return framer.ReadResponse()
}

func TestRemoteWireLostReconcileAcknowledgmentPreservesAcceptedTask(t *testing.T) {
	fixture := newRemoteServerFixture(t, true)
	origin, err := openRemoteTestReference(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	before := remoteStoreInventory(t, fixture.store.Root)
	exchange := &remoteServerExchange{deps: fixture.deps, loseOperation: remote.Reconcile}
	coordinator := remote.Coordinator{Exchange: exchange, Retry: remote.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}}
	response, err := coordinator.Reconcile(t.Context(), origin)
	if err != nil {
		t.Fatal(err)
	}
	envelope := remoteServerEnvelope(t, response)
	if exchange.calls != 2 || envelope.Publication != "committed" || !response.Existing {
		t.Fatalf("calls=%d response=%+v", exchange.calls, response)
	}
	if before != remoteStoreInventory(t, fixture.store.Root) {
		t.Fatal("terminal reconcile changed remote evidence")
	}
	if _, err = origin.Accepted(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteConcurrentTerminalReconciliationPreservesOneStart(t *testing.T) {
	fixture := newRemoteServerFixture(t, true)
	before := remoteStoreInventory(t, fixture.store.Root)
	const readers = 6
	results := make(chan remote.Response, readers)
	var group sync.WaitGroup
	for range readers {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- executeRemote(t.Context(), fixture.request(remote.Reconcile), fixture.deps)
		}()
	}
	group.Wait()
	close(results)
	for response := range results {
		envelope := remoteServerEnvelope(t, response)
		if !response.Existing || envelope.Publication != "committed" {
			t.Fatalf("reconcile=%+v", response)
		}
	}
	if before != remoteStoreInventory(t, fixture.store.Root) {
		t.Fatal("concurrent reconciliation rewrote existing evidence")
	}
}

func TestRemoteWireSnapshotTransferResumesAfterLostChunkAcknowledgment(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	sourcePath := t.TempDir()
	content := bytes.Repeat([]byte("selected-dirty-input\n"), 6000)
	if err := os.WriteFile(filepath.Join(sourcePath, "input.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	manifest, err := remotefiles.BuildManifest(t.Context(), source, []string{"input.txt"})
	if err != nil {
		t.Fatal(err)
	}
	exchange := &remoteServerExchange{deps: fixture.deps, loseOperation: remote.StageChunk}
	coordinator := remote.Coordinator{Exchange: exchange, Retry: remote.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }}}
	cwd, err := uploadRemoteInputs(t.Context(), coordinator, fixture.intent, manifest, source)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(cwd, "input.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !exchange.lost {
		t.Fatal("fault injection did not drop a chunk acknowledgment")
	}
	if !bytes.Equal(actual, content) {
		t.Fatal("staged bytes differ")
	}
	// Re-upload uses recorded complete bytes; it never changes the task identity.
	repeat, err := uploadRemoteInputs(t.Context(), coordinator, fixture.intent, manifest, source)
	if err != nil || repeat != cwd {
		t.Fatalf("repeat cwd=%q err=%v", repeat, err)
	}
}

func openRemoteTestReference(t *testing.T, fixture *remoteServerFixture) (*remote.Reference, error) {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "origin"), fixture.deps.storeDependencies(), true)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	reference, err := remote.OpenReference(t.Context(), store, fixture.intent.TaskID, true)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if closeErr := reference.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = reference.SaveIntent(t.Context(), fixture.intent, fixture.brief); err != nil {
		return nil, err
	}
	return reference, nil
}

// The real task store and canonical submission permit arbitrate this race. The
// supervisor fixture accepts commands but launches no native provider process.
func TestRemoteReconcileRacesOriginalSubmissionWithoutAnotherAdd(t *testing.T) {
	original := newOrdinarySubmissionReceiptFailureFixture(t)
	brief := []byte("ordinary submission receipt recovery")
	pin, err := original.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := *original.req
	request.TaskID = strings.Repeat("a", 32)
	identity := remote.Identity{Host: "worker", Port: 22, User: "runner", HostKey: "SHA256:fixture", Root: original.store.Root, RootID: original.store.RootID, RootDevice: pin.Device(), RootInode: pin.Inode()}
	intent := remote.Intent{Version: 1, OriginRootID: strings.Repeat("b", 32), TaskID: request.TaskID, Device: "worker", Executable: "delegate", Destination: identity, Provider: request.Provider, Config: request.RequestedConfig, RemoteCwd: request.CanonicalCwd, Brief: remote.Descriptor{Name: "brief", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)}}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	request.Remote = &task.RemoteBinding{OriginRootID: intent.OriginRootID, IntentSHA256: digest}
	metadata := original.meta
	metadata.TaskID = request.TaskID
	metadata.SpecSHA256 = ""
	td, err := original.store.CreateTask(request.TaskID, &request, brief, &metadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// The original operation holds its submission permit before its supervisor add.
	permit, err := td.PrepareSubmission(original.supervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	deps := Dependencies{ResourceChecker: fixtureResources, SupervisorOptions: original.options, RunnerExecutable: original.runner, PrepareProfile: func(saved task.TaskRecord) (PreparedProfile, error) {
		return appTestExecutionProfile(&saved, "/usr/bin/true", nil), nil
	}}
	incoming := remote.Request{Version: 1, Operation: remote.Reconcile, Root: identity.Root, Expected: &identity, TaskID: intent.TaskID, Intent: &intent, Data: brief}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result := executeRemote(ctx, incoming, deps)
	envelope := remoteServerEnvelope(t, result)
	if !result.Existing || envelope.TaskRecord != "created" {
		t.Fatalf("matching in-flight request lost: %+v", result)
	}
	assertOrdinarySubmissionCommandCount(t, original.commandLog, "add", 0)
	submitted := tasklifecycle.SubmitWithClient(td, &request, &metadata, original.supervisor, original.runner, permit)
	if submitted.Failure != nil {
		t.Fatalf("original submit failed: %v", submitted.Failure)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	writeRunnerStashedStatus(t, original.statusPath, request.RootID, request.TaskID, 7)
	result = executeRemote(t.Context(), incoming, deps)
	envelope = remoteServerEnvelope(t, result)
	if !result.Existing || envelope.Admission != "admitted" {
		t.Fatalf("reconcile lost accepted row: %+v", result)
	}
	assertOrdinarySubmissionCommandCount(t, original.commandLog, "add", 1)
}
