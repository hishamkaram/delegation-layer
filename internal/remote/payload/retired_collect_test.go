package payload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

type retiredCollectFixture struct {
	origin  *taskdir.Store
	reader  *taskdir.ReadOnlyStore
	intent  remote.Intent
	capture remotefiles.Receipt
}

func newRetiredCollectFixture(t *testing.T) retiredCollectFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	origin := supervisorfixture.NewInspectionStore(t)
	destination, seed, original := lifecyclefixture.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID = strings.Repeat("f", 32)
	intent := remote.Intent{
		Version: remote.ProtocolVersion, StorageLayout: task.RemoteDataOrganized,
		OriginRootID: origin.RootID, TaskID: request.TaskID, Device: "fixture", Executable: "delegate",
		Destination: remote.Identity{Host: "fixture", Port: 22, User: "fixture", HostKey: "SHA256:fixture", Root: destination.Root, RootID: destination.RootID},
		Provider:    request.Provider, Config: request.RequestedConfig, RemoteCwd: request.CanonicalCwd,
		Brief: remote.Descriptor{Name: "brief", Length: 5, SHA256: task.ComputeSHA256([]byte("brief"))}, OutputPaths: []string{"selected.txt"},
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	request.Remote = &task.RemoteBinding{StorageLayout: intent.StorageLayout, OriginRootID: origin.RootID, IntentSHA256: digest, OutputPaths: intent.OutputPaths}
	meta.TaskID, meta.SpecSHA256 = request.TaskID, ""
	td, err := destination.CreateTask(request.TaskID, &request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = os.WriteFile(filepath.Join(request.CanonicalCwd, "selected.txt"), []byte("selected bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	completeRetirementTask(t, td)
	capture, err := CaptureOutputs(ctx, destination, td)
	if err != nil {
		t.Fatal(err)
	}
	if report, retireErr := Retire(ctx, destination, request.TaskID, remotedata.Destination, true); retireErr != nil || !report.Applied {
		t.Fatalf("retirement=%+v %v", report, retireErr)
	}
	reference, err := remote.OpenReference(ctx, origin, request.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = reference.SaveIntent(ctx, intent, []byte("brief")); err != nil {
		t.Error(err)
	}
	if err = reference.Close(); err != nil {
		t.Fatal(err)
	}
	return retiredCollectFixture{origin: origin, reader: retirementReader(t, destination), intent: intent, capture: capture}
}

// retiredCollectExchange routes framed requests through the real destination
// reader and response codec; the origin coordinator still verifies identity.
type retiredCollectExchange struct {
	reader         *taskdir.ReadOnlyStore
	forgeName      bool
	retiredFetches int
}

func (e *retiredCollectExchange) Call(ctx context.Context, request remote.Request) (remote.Response, error) {
	var requestWire bytes.Buffer
	if err := (remote.Framer{Writer: &requestWire}).WriteRequest(request); err != nil {
		return remote.Response{}, err
	}
	received, err := (remote.Framer{Reader: &requestWire}).ReadRequest()
	if err != nil {
		return remote.Response{}, err
	}
	output := strings.HasPrefix(received.Transfer.Selector, "output:")
	if output && received.Transfer.Length > 0 {
		e.retiredFetches++
		return remote.Response{}, fmt.Errorf("retired output bytes must not be fetched")
	}
	response := Download(ctx, e.reader, received, remote.Response{Version: remote.ProtocolVersion, Identity: *received.Expected})
	if output && e.forgeName && response.Transfer != nil {
		response.Transfer.Descriptor.Name = "different.txt"
	}
	var responseWire bytes.Buffer
	if err = (remote.Framer{Writer: &responseWire}).WriteResponse(response); err != nil {
		return remote.Response{}, err
	}
	return (remote.Framer{Reader: &responseWire}).ReadResponse()
}

func TestRemoteCollectRetiredOutputKeepsPayloadAndCaptureProvenance(t *testing.T) {
	fixture := newRetiredCollectFixture(t)
	exchange := &retiredCollectExchange{reader: fixture.reader}
	out := filepath.Join(t.TempDir(), "downloads")
	reports, err := CollectFiles(t.Context(), fixture.origin, remote.Coordinator{Exchange: exchange}, fixture.intent, out)
	if err != nil {
		t.Fatalf("collect retired output: %v; reports=%+v", err, reports)
	}
	if len(reports) != 2 || reports[0].Download != "complete" {
		t.Fatalf("sealed payload unavailable: %+v", reports)
	}
	selected := reports[1]
	want := remote.Descriptor{Name: "selected.txt", Length: int64(len("selected bytes")), SHA256: task.ComputeSHA256([]byte("selected bytes"))}
	if selected.Availability != "retired" || selected.Download != "not_requested" || selected.Descriptor != want || selected.LocalPath != "" || selected.Error != "" {
		t.Fatalf("retired output=%+v", selected)
	}
	if !reflect.DeepEqual(selected.Capture, &fixture.capture.Files[0]) || !reflect.DeepEqual(selected.Provenance, &fixture.capture.Provenance) {
		t.Fatalf("retained capture identity changed: %+v", selected)
	}
	if exchange.retiredFetches != 0 {
		t.Fatal("origin requested retired bytes")
	}
	payload, err := os.ReadFile(filepath.Join(out, fixture.intent.TaskID, "result", "result.txt"))
	if err != nil || string(payload) != "answer\n" {
		t.Fatalf("sealed payload=%q %v", payload, err)
	}
	if _, err = os.Stat(filepath.Join(out, fixture.intent.TaskID, "files", "selected.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired output materialized: %v", err)
	}
}

func TestRemoteCollectRetiredOutputStillRejectsDifferentDescriptorName(t *testing.T) {
	fixture := newRetiredCollectFixture(t)
	exchange := &retiredCollectExchange{reader: fixture.reader, forgeName: true}
	reports, err := CollectFiles(t.Context(), fixture.origin, remote.Coordinator{Exchange: exchange}, fixture.intent, filepath.Join(t.TempDir(), "downloads"))
	if !errors.Is(err, remote.ErrIdentity) || len(reports) != 2 || reports[0].Download != "complete" || reports[1].Download != "failed" {
		t.Fatalf("different retired identity accepted: %+v %v", reports, err)
	}
	if exchange.retiredFetches != 0 {
		t.Fatal("origin requested retired bytes before identity verification")
	}
}
