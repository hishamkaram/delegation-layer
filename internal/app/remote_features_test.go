package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app/remotecommands"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remote/payload"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRemoteStorageAndLogArguments(t *testing.T) {
	handle := "remote:" + strings.Repeat("a", 32)
	for _, args := range [][]string{{"remote", "usage", handle, "--json"}, {"remote", "retire", handle, "--scope", "origin", "--dry-run", "--json"}, {"remote", "retire", handle, "--apply", "--json"}, {"logs", handle, "--stream", "stdout", "--limit", "65536", "--json"}, {"logs", handle, "--json"}} {
		if _, err := ParseArguments(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"remote", "retire", handle, "--apply", "--dry-run"}, {"remote", "usage", handle, "--apply"}, {"remote", "retire", handle, "--scope", "all"}, {"logs", handle, "--limit", "1"}, {"logs", handle, "--stream", "other"}, {"logs", handle, "--stream", "stdout", "--limit", "65537"}, {"logs", strings.TrimPrefix(handle, "remote:"), "--stream", "stdout"}} {
		if _, err := ParseArguments(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestRemoteLogRangesTraverseWireWithoutCacheOrAbsolutePaths(t *testing.T) {
	fixture := newRemoteServerFixture(t, true)
	before := remoteStoreInventory(t, fixture.store.Root)
	request := fixture.request(remote.Download)
	request.Intent, request.Data = nil, nil
	request.Transfer = &remote.TransferRequest{Selector: "log:descriptors"}
	response, err := (&remoteServerExchange{deps: fixture.deps}).Call(t.Context(), request)
	if err != nil || response.Failure != nil {
		t.Fatalf("metadata: %v %+v", err, response.Failure)
	}
	if bytes.Contains(response.Envelope, []byte(fixture.store.Root)) || bytes.Contains(response.Envelope, []byte("answer")) {
		t.Fatal("metadata exposed path or raw output")
	}
	request.Transfer = &remote.TransferRequest{Selector: "log:stdout", Length: 3}
	response, err = (&remoteServerExchange{deps: fixture.deps}).Call(t.Context(), request)
	if err != nil || response.Failure != nil || !bytes.Equal(response.Data, []byte("ans")) {
		t.Fatalf("range: %v %+v", err, response)
	}
	range1 := response.Transfer.Range
	if range1 == nil || !range1.Sealed || range1.RangeSHA256 != task.ComputeSHA256([]byte("ans")) {
		t.Fatalf("bad range metadata %+v", range1)
	}
	request.Transfer = &remote.TransferRequest{Selector: "log:stdout", Offset: 3, Length: remote.MaxDataBytes, Cursor: range1.Cursor}
	response, err = (&remoteServerExchange{deps: fixture.deps}).Call(t.Context(), request)
	if err != nil || response.Failure != nil || !bytes.Equal(response.Data, []byte("wer\n")) {
		t.Fatalf("next range: %v %+v", err, response)
	}
	if before != remoteStoreInventory(t, fixture.store.Root) {
		t.Fatal("log read mutated state")
	}
}

func TestRemoteStorageWholeErrorClassification(t *testing.T) {
	report := remotecommands.Report{Retirement: map[string]payload.RetirementReport{}}
	if got := remoteCLIResultCode("remote", report, io.EOF); got != 3 {
		t.Fatalf("transient exit %d", got)
	}
	if got := remoteCLIResultCode("remote", report, errors.Join(io.EOF, task.ErrEvidenceFault)); got != 1 {
		t.Fatalf("mixed fault hidden: %d", got)
	}
}

func TestRemoteAdmissionProjectsRetirementAfterWorkspaceValidationFailure(t *testing.T) {
	fixture := retiredRemoteServerFixture(t)
	var err error
	if err = os.RemoveAll(fixture.intent.RemoteCwd); err != nil {
		t.Fatal(err)
	}
	pin, err := fixture.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.request(remote.Reconcile)
	response := remote.Response{Version: remote.ProtocolVersion, Identity: fixture.intent.Destination}
	adapter := remoteLocalAdapter{deps: fixture.deps}
	// Enter the local adapter directly to cover retirement becoming visible
	// after the server's earlier pre-admission check, on this first call.
	actual := adapter.Admit(t.Context(), pin, request, &task.RemoteBinding{OriginRootID: fixture.intent.OriginRootID, IntentSHA256: fixture.digest}, response)
	envelope := remoteServerEnvelope(t, actual)
	if actual.TaskRecord != "created" || !actual.Existing || envelope.Error != "" || envelope.Publication != "committed" {
		t.Fatalf("known acceptance lost after retirement: %+v %+v", actual, envelope)
	}
	mixed := adapter.Retired(t.Context(), pin, request, actual, errors.Join(config.ErrInvalidCwd, os.ErrNotExist, task.ErrEvidenceFault))
	envelope = remoteServerEnvelope(t, mixed)
	if mixed.TaskRecord != "created" || envelope.Error == "" || envelope.Failure == nil || envelope.Failure.Code != "task_operation_failed" || remoteCLIResultCode("dispatch", remotecommands.Report{Remote: &mixed}, nil) != 1 {
		t.Fatalf("concrete joined fault hidden: %+v %+v", mixed, envelope)
	}
}

func retiredRemoteServerFixture(t *testing.T) *remoteServerFixture {
	t.Helper()
	fixture := newRemoteServerFixture(t, false)
	sealRemoteServerFixtureEvidence(t, fixture.td)
	owner, err := fixture.td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	facts := taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "retirement-fixture", BootID: "fixture-boot", Deadline: time.Now().Add(time.Minute).UTC()}
	if _, err = fixture.td.RecordLifetimeIntentContext(t.Context(), facts); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.td.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID); err != nil {
		t.Fatal(err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	if _, cleanupErr, collectErr := fixture.td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatal(errors.Join(cleanupErr, collectErr))
	}
	if report, retireErr := payload.Retire(t.Context(), fixture.store, fixture.intent.TaskID, remotedata.Destination, true); retireErr != nil || !report.Applied {
		t.Fatalf("retirement: %+v %v", report, retireErr)
	}
	return fixture
}

func TestRetirementEvidenceFaultCannotDurablyRejectExistingTask(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	journal, err := fixture.store.OpenRemoteInputJournalContext(t.Context(), fixture.intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.Put("retirement.json", struct {
		Unexpected bool `json:"unexpected"`
	}{true})
	if err = errors.Join(err, journal.Close()); err != nil {
		t.Fatal(err)
	}
	reference, err := remote.OpenReference(t.Context(), fixture.store, fixture.intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reference.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = reference.SaveIntent(t.Context(), fixture.intent, fixture.brief); err != nil {
		t.Fatal(err)
	}
	coordinator := remote.Coordinator{Exchange: &remoteServerExchange{deps: fixture.deps}}
	response, err := coordinator.Dispatch(t.Context(), reference)
	if !errors.Is(err, remote.ErrAcceptanceUnknown) || errors.Is(err, remote.ErrAdmissionRejected) || response.TaskRecord != "unknown" {
		t.Fatalf("retirement read fault became definite rejection: %+v %v", response, err)
	}
	response, err = coordinator.Reconcile(t.Context(), reference)
	if errors.Is(err, remote.ErrAdmissionRejected) || response.TaskRecord != "unknown" || response.Failure == nil || response.Failure.Code != "retirement_evidence_unavailable" {
		t.Fatalf("uncertain reference recorded a rejection: %+v %v", response, err)
	}
}

func TestRetiredReconciliationValidatesActualIntentBody(t *testing.T) {
	fixture := retiredRemoteServerFixture(t)
	request := fixture.request(remote.Reconcile)
	changed := *request.Intent
	changed.RemoteCwd = t.TempDir()
	request.Intent = &changed
	response, err := (&remoteServerExchange{deps: fixture.deps}).Call(t.Context(), request)
	if err != nil || response.Existing || response.TaskRecord == "created" || response.Failure == nil {
		t.Fatalf("claimed digest authorized a different body: %+v %v", response, err)
	}
}
