package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func TestOperationRequestCloneDetachesNestedCheckpointState(t *testing.T) {
	store := newCheckpointStore(t)
	deadline := time.Now().Add(time.Hour)
	input := ordinaryCheckpointRequest(store, testID('1'), deadline)
	operation := openCheckpointOperation(t, store, input)
	want := ordinaryCheckpointRequest(store, testID('1'), deadline)
	wantDigest := canonicalDigest(t, want)

	input.Checkpoint.Cwd = "/elsewhere"
	input.Checkpoint.CwdIdentity.Inode = 99
	returned := operation.Request()
	returned.Checkpoint.Kind = contract.CheckpointManaged
	returned.Checkpoint.CwdIdentity.Device = 98
	if got := operation.Request(); !reflect.DeepEqual(got, want) || operation.Digest() != wantDigest {
		t.Fatalf("retained request=%+v digest=%s, want %+v digest=%s", got, operation.Digest(), want, wantDigest)
	}
	loaded, err := LoadOperation(t.Context(), store, want.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOperation(t, loaded)
	if !reflect.DeepEqual(loaded.Request(), want) || loaded.Digest() != wantDigest {
		t.Fatalf("stored request=%+v digest=%s, want %+v", loaded.Request(), loaded.Digest(), want)
	}

	verify := checkpointBaseRequest(store, testID('2'), OperationVerify, deadline)
	verify.WorkspaceID, verify.Arguments = testID('3'), []string{"/bin/true", "--flag"}
	verifyOperation := openCheckpointOperation(t, store, verify)
	verify.Arguments[0] = "/bin/false"
	verifyOperation.Request().Arguments[1] = "--other"
	if got := verifyOperation.Request().Arguments; !slices.Equal(got, []string{"/bin/true", "--flag"}) {
		t.Fatalf("retained arguments=%v", got)
	}
}

func TestCheckpointCandidateCreateOnceReplayAndConflict(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	if _, err := operation.CheckpointCandidate(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("missing candidate error=%v, want absent", err)
	}
	candidate := checkpointCandidateForTest(t, operation)
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	assertCheckpointRecordBytes(t, store, operation, checkpointCandidateName, candidate)
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatalf("identical candidate replay failed: %v", err)
	}
	conflicting := candidate
	conflicting.IndexSHA256 = testSHA('9')
	if err := operation.RecordCheckpointCandidate(t.Context(), conflicting); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting candidate error=%v, want evidence fault", err)
	}
	if stored, err := operation.CheckpointCandidate(t.Context()); err != nil || stored != candidate {
		t.Fatalf("stored candidate=%+v err=%v", stored, err)
	}
}

func TestCheckpointCandidateAmbiguousWriteReplaysOneWinner(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	candidate := checkpointCandidateForTest(t, operation)
	fault := &postLinkBarrierFault{name: checkpointCandidateName}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); !errors.Is(err, task.ErrUncertainDurability) || !fault.fired {
		t.Fatalf("ambiguous candidate error=%v fired=%t", err, fault.fired)
	}
	if stored, err := operation.CheckpointCandidate(t.Context()); err != nil || stored != candidate {
		t.Fatalf("linked winner was not read back: %+v %v", stored, err)
	}
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatalf("matching replay after ambiguity failed: %v", err)
	}
	conflicting := candidate
	conflicting.TreeOID = strings.Repeat("f", 40)
	conflicting.CommitPayload = strings.Replace(conflicting.CommitPayload, candidate.TreeOID, conflicting.TreeOID, 1)
	if err := operation.RecordCheckpointCandidate(t.Context(), conflicting); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting candidate after ambiguity error=%v", err)
	}
}

func TestCheckpointCandidateBindsFullRequestDigest(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	foreign := checkpointCandidateForTest(t, operation)
	foreign.RequestSHA256 = testSHA('7')
	foreign.CommitPayload = checkpointPayloadForTest(foreign.TreeOID, foreign.ObservedHEAD, foreign.OperationID, foreign.RequestSHA256)
	if err := operation.RecordCheckpointCandidate(t.Context(), foreign); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("foreign-digest candidate error=%v", err)
	}
	if _, err := operation.CheckpointCandidate(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("refused candidate was written: %v", err)
	}
	if _, err := operation.control.PutContext(t.Context(), checkpointCandidateName, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := operation.CheckpointCandidate(t.Context()); !errors.Is(err, task.ErrEvidenceFault) || task.RecordAbsent(err) {
		t.Fatalf("stored foreign-digest candidate error=%v, want evidence fault", err)
	}

	verify := checkpointBaseRequest(store, testID('2'), OperationVerify, time.Now().Add(time.Hour))
	verify.WorkspaceID, verify.Arguments = testID('3'), []string{"/bin/true"}
	other := openCheckpointOperation(t, store, verify)
	if _, err := other.CheckpointCandidate(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("non-checkpoint candidate read error=%v", err)
	}
}

// TestCheckpointRecordsAfterClosureNeedNoLaunchPermission proves the
// candidate is fenced by the live launch permission while the qualified
// receipt is persisted after closure without any launch claim. Closure here
// is the unlaunched fence; qualifying the receipt's closure stays with the
// coordinator.
func TestCheckpointRecordsAfterClosureNeedNoLaunchPermission(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(-time.Second)))
	candidate := checkpointCandidateForTest(t, operation)
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := operation.Claim(t.Context(), "start.json"); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("closed lifetime granted a launch claim: %v", err)
	}
	changed := candidate
	changed.IndexSHA256 = testSHA('8')
	if err := operation.RecordCheckpointCandidate(t.Context(), changed); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("closed lifetime accepted a different candidate: %v", err)
	}
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatalf("closed lifetime refused exact candidate replay: %v", err)
	}
	receipt := checkpointedReceiptForTest(operation, candidate)
	if err := operation.RecordCheckpointReceipt(t.Context(), receipt); err != nil {
		t.Fatalf("post-closure receipt failed: %v", err)
	}
	assertCheckpointRecordBytes(t, store, operation, checkpointReceiptName, receipt)
	if stored, err := operation.CheckpointReceipt(t.Context()); err != nil || !reflect.DeepEqual(stored, receipt) {
		t.Fatalf("stored receipt=%+v err=%v", stored, err)
	}
	if err := operation.RecordCheckpointReceipt(t.Context(), receipt); err != nil {
		t.Fatalf("matching receipt replay failed: %v", err)
	}
	unavailable := unavailableCheckpointReceipt(operation, contract.CheckpointFailed)
	if err := operation.RecordCheckpointReceipt(t.Context(), unavailable); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting receipt error=%v", err)
	}
}

func TestCheckpointCandidateRefusedWithoutLiveLaunchPermission(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(-time.Second)))
	if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordCheckpointCandidate(t.Context(), checkpointCandidateForTest(t, operation)); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("closed lifetime created a candidate: %v", err)
	}
	if _, err := operation.CheckpointCandidate(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("candidate read fabricated a record: %v", err)
	}
}

func TestCheckpointReceiptBindsStoredCandidate(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	candidate := checkpointCandidateForTest(t, operation)
	receipt := checkpointedReceiptForTest(operation, candidate)
	if err := operation.RecordCheckpointReceipt(t.Context(), receipt); !errors.Is(err, task.ErrEvidenceFault) || task.RecordAbsent(err) {
		t.Fatalf("receipt without stored candidate error=%v", err)
	}
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	different := candidate
	different.IndexSHA256 = testSHA('6')
	if err := operation.RecordCheckpointReceipt(t.Context(), checkpointedReceiptForTest(operation, different)); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("receipt with a different candidate error=%v", err)
	}
	if _, err := operation.control.PutContext(t.Context(), checkpointReceiptName, checkpointedReceiptForTest(operation, different)); err != nil {
		t.Fatal(err)
	}
	if _, err := operation.CheckpointReceipt(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("stored receipt with a different candidate error=%v", err)
	}
}

func TestCheckpointUnavailableReceiptKeepsDurableCandidate(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	candidate := checkpointCandidateForTest(t, operation)
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	receipt := unavailableCheckpointReceipt(operation, contract.CheckpointResultUnavailable)
	if err := operation.RecordCheckpointReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if stored, err := operation.CheckpointCandidate(t.Context()); err != nil || stored != candidate {
		t.Fatalf("unavailable receipt hid the durable candidate: %+v %v", stored, err)
	}
	if _, err := operation.Result(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("receipt fabricated an operation result: %v", err)
	}
}

// TestHistoricalCaptureClosuresStayAcceptedWhileCheckpointSuccessIsNative
// pairs each historical capture closure that remains accepted with the
// refusal of the same closure for a new checkpoint success.
func TestHistoricalCaptureClosuresStayAcceptedWhileCheckpointSuccessIsNative(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	source := testSource(t)
	recordResourceSource(t, resource, source)
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(time.Hour)))
	candidate := checkpointCandidateForTest(t, operation)
	if err := operation.RecordCheckpointCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	digest := testSHA('5')
	historical := []struct {
		name    string
		closure func(*CaptureReceipt)
	}{
		{name: "previous boot", closure: previousBootCaptureClosure(digest)},
		{name: "native", closure: nativeCaptureClosure(digest, digest)},
	}
	for index, row := range historical {
		operationID := testID(byte('a' + index))
		capture := candidateForTest(resource, source, subjectFor('c'), operationID)
		if err := resource.RecordCaptureCandidate(t.Context(), capture); err != nil {
			t.Fatal(err)
		}
		captured := CaptureReceipt{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: operationID, Subject: capture.Subject, Status: "captured", Candidate: &capture}
		row.closure(&captured)
		if err := resource.RecordCaptureReceipt(t.Context(), captured); err != nil {
			t.Fatalf("%s: historical capture refused: %v", row.name, err)
		}
	}
	refused := map[string]func(*contract.CheckpointReceipt){
		"previous boot": func(receipt *contract.CheckpointReceipt) {
			receipt.LifetimeClosure, receipt.WorkerWaitSHA256 = task.LifetimeClosureLegacyPreviousBoot, ""
		},
		"unregistered": func(receipt *contract.CheckpointReceipt) {
			receipt.LifetimeClosure, receipt.TerminationSHA256 = task.LifetimeClosureUnregisteredRecovered, ""
		},
		"unlaunched": func(receipt *contract.CheckpointReceipt) {
			receipt.TerminationSHA256, receipt.WorkerWaitSHA256, receipt.UnlaunchedSHA256 = "", "", digest
		},
	}
	for name, mutate := range refused {
		receipt := checkpointedReceiptForTest(operation, candidate)
		mutate(&receipt)
		if err := operation.RecordCheckpointReceipt(t.Context(), receipt); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s: checkpoint success error=%v, want identity mismatch", name, err)
		}
	}
	if _, err := operation.CheckpointReceipt(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("refused checkpoint success was written: %v", err)
	}
}

func TestResourceOperationReferencesAttachOnlyManagedCheckpoints(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	defer closeResource(t, resource)
	deadline := time.Now().Add(time.Hour)
	managed := checkpointBaseRequest(store, testID('1'), contract.OperationCheckpoint, deadline)
	managed.WorkspaceID, managed.Checkpoint = request.WorkspaceID, &contract.CheckpointSelection{Kind: contract.CheckpointManaged}
	if err := resource.RecordOperation(t.Context(), managed); err != nil {
		t.Fatalf("managed checkpoint reference refused: %v", err)
	}
	reference := OperationReference{SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID, OperationID: managed.OperationID, Kind: contract.OperationCheckpoint}
	assertRecordBytes(t, filepath.Join(store.Root, "workspaces", request.WorkspaceID, operationRecordName(managed.OperationID)), reference)
	if ids, err := resource.OperationIDs(t.Context()); err != nil || !slices.Equal(ids, []string{managed.OperationID}) {
		t.Fatalf("operation ids=%v err=%v", ids, err)
	}

	otherWorkspace := managed
	otherWorkspace.OperationID, otherWorkspace.WorkspaceID = testID('2'), testID('9')
	ordinary := ordinaryCheckpointRequest(store, testID('3'), deadline)
	original := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('4'), deadline))
	removal := removalRequestForTest(store, testID('5'), original)
	for name, refused := range map[string]OperationRequest{"other workspace": otherWorkspace, "ordinary": ordinary, "removal": removal} {
		if err := resource.RecordOperation(t.Context(), refused); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s checkpoint reference error=%v", name, err)
		}
	}
	forged := OperationReference{SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID, OperationID: removal.OperationID, Kind: contract.OperationCheckpointRemove}
	if _, err := resource.control.PutContext(t.Context(), operationRecordName(removal.OperationID), forged); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.OperationIDs(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("forged removal reference error=%v", err)
	}
}

type postLinkBarrierFault struct {
	taskdir.BaseFaultInjector
	name  string
	fired bool
}

func (f *postLinkBarrierFault) OnPostLinkDirBarrier(destPath string) error {
	if f.fired || filepath.Base(destPath) != f.name {
		return nil
	}
	f.fired = true
	return errors.New("injected post-link barrier failure")
}

func newCheckpointStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func openCheckpointOperation(t *testing.T, store *taskdir.Store, request OperationRequest) *Operation {
	t.Helper()
	operation, err := OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOperation(t, operation) })
	return operation
}

func closeOperation(t *testing.T, operation *Operation) {
	t.Helper()
	if err := operation.Close(); err != nil {
		t.Error(err)
	}
}

func checkpointBaseRequest(store *taskdir.Store, operationID, kind string, deadline time.Time) OperationRequest {
	return OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: operationID, Kind: kind, Subject: subjectFor('a'),
		Deadline: deadline.UTC().Format(time.RFC3339Nano), WorkerExecutable: "/bin/worker", WorkerSHA256: testSHA('d'),
		Supervisor: task.SupervisorRef{
			ClientExecutable: "/fixture/pueue-client", ClientSHA256: testSHA('b'), ResolvedConfigSHA256: testSHA('c'),
			Endpoint: "unix:/fixture/pueue.sock", ConfigPath: "/fixture/pueue.conf", ConfigDigest: testSHA('1'), ObservedVersion: "fixture",
		},
	}
}

func ordinaryCheckpointRequest(store *taskdir.Store, operationID string, deadline time.Time) OperationRequest {
	request := checkpointBaseRequest(store, operationID, contract.OperationCheckpoint, deadline)
	request.Checkpoint = &contract.CheckpointSelection{Kind: contract.CheckpointOrdinary, Cwd: "/repo/sub", CwdIdentity: &DirectoryIdentity{Device: 1, Inode: 2}}
	return request
}

func checkpointPayloadForTest(tree, parent, operationID, requestSHA256 string) string {
	return "tree " + tree + "\nparent " + parent +
		"\nauthor Delegate <delegate@localhost> 1761048000 +0000\ncommitter Delegate <delegate@localhost> 1761048000 +0000\n\n" +
		"Delegate checkpoint\n\n" + contract.CheckpointTrailer + operationID + "\n" + contract.CheckpointRequestTrailer + requestSHA256 + "\n"
}

func checkpointCandidateForTest(t *testing.T, operation *Operation) contract.CheckpointCandidate {
	t.Helper()
	request := operation.Request()
	ref, err := contract.CheckpointRef(request.RootID, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Root: "/repo", CommonDir: "/repo/.git", GitDir: "/repo/.git", RelativeCwd: "sub", BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1"}
	head, tree := strings.Repeat("b", 40), strings.Repeat("c", 40)
	return contract.CheckpointCandidate{
		SchemaVersion: task.SchemaVersion, OperationID: request.OperationID, RequestSHA256: operation.Digest(),
		Target:       contract.CheckpointTarget{Source: source.WithCommonDirIdentity(DirectoryIdentity{Device: 1, Inode: 2}), CheckoutRoot: "/repo", AdminDir: "/repo/.git"},
		ObservedHEAD: head, IndexSHA256: testSHA('e'), TreeOID: tree, CommitOID: strings.Repeat("d", 40), SnapshotRef: ref,
		CommitPayload: checkpointPayloadForTest(tree, head, request.OperationID, operation.Digest()),
	}
}

func checkpointedReceiptForTest(operation *Operation, candidate contract.CheckpointCandidate) contract.CheckpointReceipt {
	return contract.CheckpointReceipt{
		SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(),
		Status: contract.CheckpointCheckpointed, Candidate: &candidate, TerminationSHA256: testSHA('1'), WorkerWaitSHA256: testSHA('2'),
	}
}

func unavailableCheckpointReceipt(operation *Operation, reason string) contract.CheckpointReceipt {
	return contract.CheckpointReceipt{
		SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(),
		Status: contract.CheckpointUnavailable, ReasonCode: reason, TerminationSHA256: testSHA('1'), WorkerWaitSHA256: testSHA('2'),
	}
}

func canonicalDigest(t *testing.T, record OperationRequest) string {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	return task.ComputeSHA256(data)
}

func assertCheckpointRecordBytes[Record any](t *testing.T, store *taskdir.Store, operation *Operation, name string, record Record) {
	t.Helper()
	assertRecordBytes(t, filepath.Join(store.Root, "workspace-operations", operation.Request().OperationID, name), record)
}

// assertRecordBytes compares the durable file with the record's exact
// canonical JSON encoding.
func assertRecordBytes[Record any](t *testing.T, path string, record Record) {
	t.Helper()
	want, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s=%s err=%v, want %s", filepath.Base(path), got, err, want)
	}
}
