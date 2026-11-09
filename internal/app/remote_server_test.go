package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type remoteServerFixture struct {
	digest string
	store  *taskdir.Store
	td     *taskdir.TaskDir
	intent remote.Intent
	brief  []byte
	deps   Dependencies
}

func newRemoteServerFixture(t *testing.T, sealed bool) *remoteServerFixture {
	t.Helper()
	store, seed, original := newAppTestTask(t, false)
	_, metadata, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	identity := remote.Identity{Host: "worker.example", Port: 22, User: "runner", HostKey: "SHA256:fixture", Root: store.Root, RootID: store.RootID, RootDevice: pin.Device(), RootInode: pin.Inode()}
	brief := []byte("brief")
	intent := remote.Intent{Version: 1, OriginRootID: strings.Repeat("b", 32), TaskID: strings.Repeat("a", 32), Device: "worker", Executable: "delegate", Destination: identity, Provider: original.Provider, Config: original.RequestedConfig, RemoteCwd: original.CanonicalCwd, Brief: remote.Descriptor{Name: "brief", Length: 5, SHA256: task.ComputeSHA256(brief)}}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID = intent.TaskID
	request.Remote = &task.RemoteBinding{OriginRootID: intent.OriginRootID, IntentSHA256: digest}
	meta := *metadata
	meta.TaskID = request.TaskID
	meta.SpecSHA256 = ""
	td, err := store.CreateTask(request.TaskID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if sealed {
		sealRemoteServerFixture(t, td)
	}
	return &remoteServerFixture{digest: digest, store: store, td: td, intent: intent, brief: brief, deps: Dependencies{ResourceChecker: fixtureResources}}
}

func sealRemoteServerFixture(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	sealRemoteServerFixtureEvidence(t, td)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatal(errors.Join(cleanupErr, collectErr))
	}
}

func sealRemoteServerFixtureEvidence(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles("answer\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
}

func (f *remoteServerFixture) request(operation remote.Operation) remote.Request {
	digest := f.digest
	return remote.Request{Version: 1, Operation: operation, Root: f.store.Root, Expected: &f.intent.Destination, TaskID: f.intent.TaskID, Intent: &f.intent, IntentSHA256: digest, Data: f.brief}
}

func remoteServerEnvelope(t *testing.T, response remote.Response) Response {
	t.Helper()
	if response.Failure != nil {
		t.Fatalf("remote operation failed: %+v", response.Failure)
	}
	var envelope Response
	if err := json.Unmarshal(response.Envelope, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestRemoteHelloDoesNotInitializeMissingState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	response := remoteFeature(Dependencies{}).Execute(t.Context(), remote.Request{Version: 1, Operation: remote.Hello, Root: root})
	if response.Failure == nil || response.Failure.Code != "root_unavailable" {
		t.Fatalf("response=%+v", response)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive hello initialized state: %v", err)
	}
}

func TestRemoteReconcileAbsentRequestCannotCreateWork(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	request := fixture.request(remote.Reconcile)
	request.TaskID = strings.Repeat("d", 32)
	intent := fixture.intent
	intent.TaskID = request.TaskID
	request.Intent = &intent
	response := remoteFeature(fixture.deps).Execute(t.Context(), request)
	envelope := remoteServerEnvelope(t, response)
	if response.Existing || envelope.TaskRecord != "unknown" {
		t.Fatalf("absent request gained authority: %+v", response)
	}
	if _, err := os.Lstat(filepath.Join(fixture.store.Root, "tasks", request.TaskID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconcile created absent task: %v", err)
	}
}

func TestRemoteReconcileRejectsConflictingImmutableIntent(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	request := fixture.request(remote.Reconcile)
	intent := fixture.intent
	intent.Config.Model = "changed"
	request.Intent = &intent
	before, err := os.ReadFile(filepath.Join(fixture.td.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	response := remoteFeature(fixture.deps).Execute(t.Context(), request)
	if response.Failure == nil || response.Failure.Code != "request_conflict" {
		t.Fatalf("conflict not classified: %+v", response.Failure)
	}
	var envelope Response
	if decodeErr := json.Unmarshal(response.Envelope, &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if response.Existing || envelope.TaskRecord != "unknown" || envelope.Error == "" {
		t.Fatalf("conflicting request accepted: %+v", response)
	}
	after, err := os.ReadFile(filepath.Join(fixture.td.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("reconcile replaced immutable request")
	}
	if _, err = fixture.td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting request submitted: %v", err)
	}
}

func TestRemoteRootPinChangePermitsPassiveReadAndRefusesCancellation(t *testing.T) {
	fixture := newRemoteServerFixture(t, true)
	request := fixture.request(remote.Status)
	changed := *request.Expected
	changed.RootInode++
	request.Expected = &changed
	before := remoteStoreInventory(t, fixture.store.Root)
	response := remoteFeature(fixture.deps).Execute(t.Context(), request)
	envelope := remoteServerEnvelope(t, response)
	if !response.RootPinChanged || envelope.Publication != "committed" {
		t.Fatalf("passive read lost terminal outcome: %+v", response)
	}
	after := remoteStoreInventory(t, fixture.store.Root)
	if before != after {
		t.Fatal("root-pin passive observation wrote state")
	}
	request.Operation = remote.Cancel
	request.Control = &remote.Control{ID: strings.Repeat("e", 32)}
	response = remoteFeature(fixture.deps).Execute(t.Context(), request)
	if response.Failure == nil || response.Failure.Code != "root_pin_changed" {
		t.Fatalf("mutation allowed after pin change: %+v", response)
	}
	if before != remoteStoreInventory(t, fixture.store.Root) {
		t.Fatal("refused mutation wrote state")
	}
}

func remoteStoreInventory(t *testing.T, root string) string {
	t.Helper()
	var inventory strings.Builder
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		inventory.WriteString(path)
		inventory.WriteByte('\n')
		inventory.Write(data)
		inventory.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return inventory.String()
}

// A channel exchange serializes actual protocol frames, then calls the real
// server composition. It proves the wire/store boundary, not native SSH logout.
type remoteServerExchange struct {
	deps          Dependencies
	calls         int
	loseOperation remote.Operation
	lost          bool
}

func (e *remoteServerExchange) Call(ctx context.Context, request remote.Request) (remote.Response, error) {
	return e.exchange(ctx, request)
}

func TestRemoteReconcileDirectoryWithoutRequestRemainsUnknown(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	requestPath := filepath.Join(fixture.td.Dir, "task.json")
	if err := os.Remove(requestPath); err != nil {
		t.Fatal(err)
	}
	response := remoteFeature(fixture.deps).Execute(t.Context(), fixture.request(remote.Reconcile))
	envelope := remoteServerEnvelope(t, response)
	if response.Existing || envelope.TaskRecord != "unknown" {
		t.Fatalf("directory became receipt: %+v", response)
	}
	if _, err := os.Lstat(requestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing request was recreated: %v", err)
	}
	if _, err := fixture.td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial directory submitted: %v", err)
	}
}
