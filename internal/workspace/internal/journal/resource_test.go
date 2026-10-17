package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestResourceRegistryReplayAndCollision(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	if ids, err := ResourceIDs(t.Context(), store); err != nil || !slices.Equal(ids, []string{request.WorkspaceID}) {
		t.Fatalf("initial resource inventory ids=%v err=%v", ids, err)
	}

	replayRequest := request
	replayRequest.WorkspaceID = testID('d')
	replay, err := EstablishResource(t.Context(), store, replayRequest)
	if err != nil {
		t.Fatalf("same-task replay failed: %v", err)
	}
	t.Cleanup(func() { closeResource(t, replay) })
	if replay.Request().WorkspaceID != request.WorkspaceID {
		t.Fatalf("replay adopted caller workspace %q, want registry winner %q", replay.Request().WorkspaceID, request.WorkspaceID)
	}
	changedInput := request
	changedInput.RequestSHA256 = testSHA('e')
	if _, err = EstablishResource(t.Context(), store, changedInput); !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("same-workspace changed request error=%v, want request conflict", err)
	}

	collision := request
	collision.RequestSHA256 = testSHA('e')
	collision.WorkspaceID = testID('f')
	if _, err = EstablishResource(t.Context(), store, collision); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("different request did not collide: %v", err)
	}

	closeResource(t, resource)
}

func TestResourceBindAdmissionReplayPreservesInitialWorkspaceBinding(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	defer closeResource(t, resource)
	attachmentTask := testID('d')
	attachmentDigest := testSHA('e')
	if err := resource.BindAdmission(t.Context(), attachmentTask, attachmentDigest); err != nil {
		t.Fatal(err)
	}
	if err := resource.BindAdmission(t.Context(), attachmentTask, attachmentDigest); err != nil {
		t.Fatalf("identical attachment replay failed: %v", err)
	}
	ids, err := ResourceIDs(t.Context(), store)
	if err != nil || !slices.Equal(ids, []string{request.WorkspaceID}) {
		t.Fatalf("resource ids=%v err=%v", ids, err)
	}
	workspaceRecord := readInventoryForTest(t, store, workspaceInventoryName(request.WorkspaceID))
	if workspaceRecord.TaskID != request.TaskID || workspaceRecord.RequestSHA256 != request.RequestSHA256 {
		t.Fatalf("initial workspace binding changed: %+v", workspaceRecord)
	}
	attachmentRecord := readInventoryForTest(t, store, taskInventoryName(attachmentTask))
	if attachmentRecord.WorkspaceID != request.WorkspaceID || attachmentRecord.TaskID != attachmentTask || attachmentRecord.RequestSHA256 != attachmentDigest {
		t.Fatalf("attachment binding=%+v", attachmentRecord)
	}
}

func TestResourceBindAdmissionRejectsTaskConflictsAcrossWorkspaces(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	defer closeResource(t, resource)
	attachmentTask := testID('f')
	attachmentDigest := testSHA('a')
	if err := resource.BindAdmission(t.Context(), attachmentTask, attachmentDigest); err != nil {
		t.Fatal(err)
	}
	if err := resource.BindAdmission(t.Context(), attachmentTask, testSHA('b')); !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("same-workspace digest conflict=%v", err)
	}
	secondRequest := request
	secondRequest.WorkspaceID = testID('c')
	secondRequest.TaskID = testID('g')
	second, err := EstablishResource(t.Context(), store, secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResource(t, second)
	if err = second.BindAdmission(t.Context(), attachmentTask, attachmentDigest); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("cross-workspace task conflict=%v", err)
	}
	got := readInventoryForTest(t, store, taskInventoryName(attachmentTask))
	if got.WorkspaceID != request.WorkspaceID || got.RequestSHA256 != attachmentDigest {
		t.Fatalf("conflict overwrote task binding: %+v", got)
	}
}

func TestResourceConcurrentBindAdmissionUsesOneCanonicalStoreWinner(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	defer closeResource(t, resource)
	loaded, err := LoadResource(t.Context(), store, request.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResource(t, loaded)

	replayTask := testID('h')
	replayDigest := testSHA('i')
	replayErrs := concurrentAdmissions(resource, loaded, replayTask, replayDigest, 8)
	for _, err = range replayErrs {
		if err != nil {
			t.Fatalf("concurrent identical admission=%v", err)
		}
	}
	for index, character := range []byte{'j', 'k', 'l', 'm'} {
		admissionErrs := concurrentAdmissions(resource, loaded, testID(character), testSHA(byte('n'+index)), 2)
		for _, err = range admissionErrs {
			if err != nil {
				t.Fatalf("concurrent attachment %d=%v", index, err)
			}
		}
	}
	ids, err := ResourceIDs(t.Context(), store)
	if err != nil || !slices.Equal(ids, []string{request.WorkspaceID}) {
		t.Fatalf("resource ids=%v err=%v", ids, err)
	}
	if got := readInventoryForTest(t, store, workspaceInventoryName(request.WorkspaceID)); got.TaskID != request.TaskID {
		t.Fatalf("concurrent admissions changed initial workspace task: %+v", got)
	}
}

func concurrentAdmissions(first, second *Resource, taskID, requestSHA256 string, count int) []error {
	results := make(chan error, count)
	for index := 0; index < count; index++ {
		go func(index int) {
			resource := first
			if index%2 == 1 {
				resource = second
			}
			results <- resource.BindAdmission(context.Background(), taskID, requestSHA256)
		}(index)
	}
	errList := make([]error, 0, count)
	for index := 0; index < count; index++ {
		errList = append(errList, <-results)
	}
	return errList
}

func readInventoryForTest(t *testing.T, store *taskdir.Store, name string) inventoryRecord {
	t.Helper()
	registry, err := store.OpenWorkspaceRegistry(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	var record inventoryRecord
	err = registry.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		var readErr error
		record, readErr = readInventoryRecordTx(tx, name, store.RootID)
		return readErr
	})
	err = errors.Join(err, registry.Close())
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestResourceIDsRejectInvalidAndUnreadableInventoryRecords(t *testing.T) {
	store, resource, _ := newResourceFixture(t)
	closeResource(t, resource)

	registry, err := store.OpenWorkspaceRegistry(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	invalidID := testID('g')
	additional := inventoryRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		WorkspaceID:   invalidID,
		TaskID:        testID('h'),
		RequestSHA256: testSHA('i'),
	}
	_, err = registry.Put(workspaceInventoryName(invalidID), additional)
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Put(taskInventoryName(additional.TaskID), additional)
	if err != nil {
		t.Fatal(err)
	}
	orphan := inventoryRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		WorkspaceID:   testID('k'),
		TaskID:        testID('l'),
		RequestSHA256: testSHA('m'),
	}
	if _, err = registry.Put(taskInventoryName(orphan.TaskID), orphan); err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = ResourceIDs(t.Context(), store); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("orphan inventory was not rejected: %v", err)
	}
	registry, err = store.OpenWorkspaceRegistry(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Put(workspaceInventoryName(orphan.WorkspaceID), orphan); err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = ResourceIDs(t.Context(), store); err != nil {
		t.Fatalf("matching inventory was rejected after adding valid records: %v", err)
	}

	badName := workspaceInventoryName(testID('j'))
	if err = os.WriteFile(filepath.Join(store.Root, "workspace-registry", badName), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = ResourceIDs(t.Context(), store); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unreadable inventory record was not classified as evidence: %v", err)
	}
}

func TestResourceReservationReplayClosureAndRelease(t *testing.T) {
	store, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	exerciseReservationLifecycle(t, resource)
	exerciseReleaseLifecycle(t, store, resource)
}

func exerciseReservationLifecycle(t *testing.T, resource *Resource) {
	t.Helper()
	if _, err := resource.Reservation(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty reservation journal error=%v", err)
	}
	firstSubject := Subject{Kind: SubjectTask, ID: testID('k'), SHA256: testSHA('l')}
	first, err := resource.Reserve(t.Context(), firstSubject)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := resource.Reserve(t.Context(), firstSubject)
	if err != nil || replay != first {
		t.Fatalf("same-subject reservation replay=%+v err=%v want=%+v", replay, err, first)
	}
	other := Subject{Kind: SubjectOperation, ID: testID('m'), SHA256: testSHA('n')}
	if _, err = resource.Reserve(t.Context(), other); !errors.Is(err, ErrBusy) {
		t.Fatalf("different active subject error=%v", err)
	}
	if err = resource.CloseReservation(t.Context(), first, "aborted-not-launched", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = resource.Reservation(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed reservation still active: %v", err)
	}
	closed, err := resource.ReservationClosed(t.Context(), first.Sequence)
	if err != nil || closed.Disposition != "aborted-not-launched" {
		t.Fatalf("closure=%+v err=%v", closed, err)
	}
	second, err := resource.Reserve(t.Context(), other)
	if err != nil || second.Sequence != first.Sequence+1 {
		t.Fatalf("next reservation=%+v err=%v", second, err)
	}
}

func exerciseReleaseLifecycle(t *testing.T, store *taskdir.Store, resource *Resource) {
	t.Helper()
	release := ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: testID('o')}
	if err := resource.RecordRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordRelease(t.Context(), release); err != nil {
		t.Fatalf("matching release replay failed: %v", err)
	}
	if _, err := resource.Reserve(t.Context(), Subject{Kind: SubjectAdmission, ID: testID('p'), SHA256: testSHA('q')}); !errors.Is(err, ErrReleased) {
		t.Fatalf("release did not block reserve: %v", err)
	}
	conflict := release
	conflict.OperationID = testID('r')
	if err := resource.RecordRelease(t.Context(), conflict); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting release was accepted: %v", err)
	}
	exerciseRemovedLifecycle(t, resource, release)
	if err := os.WriteFile(filepath.Join(store.Root, "workspaces", resource.Request().WorkspaceID, resourceReleaseName), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Release(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unreadable release record was not reported as evidence fault: %v", err)
	}
}

func exerciseRemovedLifecycle(t *testing.T, resource *Resource, release ReleaseRecord) {
	t.Helper()
	active, err := resource.Reservation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.CloseReservation(t.Context(), active, "operation-completed", ""); err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordRemoved(t.Context(), release.OperationID); err != nil {
		t.Fatal(err)
	}
	if got, readErr := resource.Removed(t.Context()); readErr != nil || got.OperationID != release.OperationID {
		t.Fatalf("removed=%+v err=%v", got, readErr)
	}
	if err = resource.RecordRemoved(t.Context(), testID('s')); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting removed record was accepted: %v", err)
	}
}

func TestResourceConcurrentReserveHasOneSubject(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	subjects := []Subject{
		{Kind: SubjectTask, ID: testID('s'), SHA256: testSHA('t')},
		{Kind: SubjectOperation, ID: testID('u'), SHA256: testSHA('v')},
	}
	results := make(chan error, len(subjects))
	var wait sync.WaitGroup
	for _, subject := range subjects {
		subject := subject
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := resource.Reserve(context.Background(), subject)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	var successes, busy int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrBusy):
			busy++
		default:
			t.Fatalf("unexpected concurrent reserve error: %v", err)
		}
	}
	if successes != 1 || busy != 1 {
		t.Fatalf("concurrent reserve successes=%d busy=%d", successes, busy)
	}
}

func TestResourceSourceCheckoutRegistrationAndCapture(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	source := testSource(t)
	recordResourceSource(t, resource, source)
	checkout := recordResourceCheckout(t, resource, source)
	recordResourceRegistration(t, resource, source, checkout)
	recordResourceCapture(t, resource, source)
}

func recordResourceSource(t *testing.T, resource *Resource, source Source) {
	t.Helper()
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if got, err := resource.Source(t.Context()); err != nil || got != source {
		t.Fatalf("source=%+v err=%v", got, err)
	}
	badSource := source
	badSource.BaseOID = strings.Repeat("a", 39)
	if err := resource.RecordSource(t.Context(), badSource); err == nil {
		t.Fatal("invalid source OID was accepted")
	}
}

func recordResourceCheckout(t *testing.T, resource *Resource, source Source) Checkout {
	t.Helper()
	checkoutPath := filepath.Join(t.TempDir(), "checkout")
	checkout := Checkout{Source: source, CheckoutPath: checkoutPath, Workdir: checkoutPath, AdminDir: filepath.Join(checkoutPath, "admin"), AdminBacklink: filepath.Join(checkoutPath, "admin")}
	if err := resource.RecordCheckout(t.Context(), checkout); err != nil {
		t.Fatal(err)
	}
	if got, err := resource.Checkout(t.Context()); err != nil || got != checkout {
		t.Fatalf("checkout=%+v err=%v", got, err)
	}
	return checkout
}

func recordResourceRegistration(t *testing.T, resource *Resource, source Source, checkout Checkout) {
	t.Helper()
	registration := Registration{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID,
		SourceRoot: source.Root, CommonDir: source.CommonDir, GitDir: source.GitDir,
		AdminDir: checkout.AdminDir, AdminBacklink: checkout.AdminBacklink,
		CheckoutPath: checkout.CheckoutPath, CanonicalCwd: checkout.Workdir,
		RelativeCwd: source.RelativeCwd, BaseOID: source.BaseOID, ObjectFormat: source.ObjectFormat,
		BaseRef: "HEAD", ContainerIdentity: DirectoryIdentity{Device: 1, Inode: 2}, CheckoutIdentity: DirectoryIdentity{Device: 3, Inode: 4},
	}
	if err := resource.RecordRegistration(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Registration(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func recordResourceCapture(t *testing.T, resource *Resource, source Source) {
	t.Helper()
	operationID := testID('w')
	subject := Subject{Kind: SubjectTask, ID: testID('x'), SHA256: testSHA('y')}
	candidate := CaptureCandidate{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: operationID,
		Subject: subject, BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("b", 40), TreeOID: strings.Repeat("c", 40),
		SnapshotRef: "refs/delegate/capture-" + operationID, ObjectFormat: source.ObjectFormat,
	}
	if err := resource.RecordCaptureCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	receipt := CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: operationID,
		Subject: subject, Status: "captured", Candidate: &candidate,
		Artifacts:         []Artifact{{Name: "patch", Length: 5, SHA256: task.ComputeSHA256([]byte("patch"))}},
		TerminationSHA256: testSHA('z'), WorkerWaitSHA256: testSHA('a'),
	}
	if err := resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if got, err := resource.CaptureReceipt(t.Context(), operationID); err != nil || got.OperationID != operationID {
		t.Fatalf("receipt=%+v err=%v", got, err)
	}
	ids, err := resource.CaptureIDs(t.Context())
	if err != nil || !slices.Equal(ids, []string{operationID}) {
		t.Fatalf("capture ids=%v err=%v", ids, err)
	}
	badReceipt := receipt
	badReceipt.ReasonCode = "different"
	if err = resource.RecordCaptureReceipt(t.Context(), badReceipt); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting capture receipt error=%v", err)
	}
}

func TestResourceOperationAndForgetRecoveryRecords(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	defer closeResource(t, resource)
	operationID := testID('c')
	operation := OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: operationID,
		WorkspaceID: request.WorkspaceID, Kind: OperationForget, Subject: subjectFor('d'),
		Deadline: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), WorkerExecutable: "/bin/worker", WorkerSHA256: testSHA('e'),
		Supervisor: task.SupervisorRef{ClientExecutable: "/fixture/pueue-client", ClientSHA256: testSHA('g'), ResolvedConfigSHA256: testSHA('h'), Endpoint: "unix:/fixture/pueue.sock", ConfigPath: "/fixture/pueue.conf", ConfigDigest: testSHA('f'), ObservedVersion: "fixture"},
	}
	if err := resource.RecordOperation(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	if ids, err := resource.OperationIDs(t.Context()); err != nil || !slices.Equal(ids, []string{operationID}) {
		t.Fatalf("operation ids=%v err=%v", ids, err)
	}

	intent := ForgetIntent{
		BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID, OperationID: operationID,
		Refs:      []ForgetRef{{Ref: "refs/delegate/capture-" + operationID, ExpectedOID: strings.Repeat("f", 40)}},
		Artifacts: []ForgetArtifact{{OperationID: operationID, Name: "patch", Length: 5, SHA256: task.ComputeSHA256([]byte("patch"))}},
	}
	if err := resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if got, err := resource.ForgetIntent(t.Context()); err != nil || len(got.Refs) != 1 || len(got.Artifacts) != 1 {
		t.Fatalf("intent=%+v err=%v", got, err)
	}
	if _, err := resource.Forgotten(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial forget reported terminal: %v", err)
	}
	if err := resource.RecordForgotten(t.Context(), testID('g')); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("wrong forget operation accepted: %v", err)
	}
	if _, err := resource.ForgetIntent(t.Context()); err != nil {
		t.Fatalf("intent disappeared after partial completion: %v", err)
	}
	if err := resource.RecordForgotten(t.Context(), operationID); err != nil {
		t.Fatal(err)
	}
	if got, err := resource.Forgotten(t.Context()); err != nil || got.OperationID != operationID {
		t.Fatalf("forgotten=%+v err=%v", got, err)
	}
	if err := resource.RecordForgotten(t.Context(), operationID); err != nil {
		t.Fatalf("matching forgotten replay failed: %v", err)
	}
}

func newResourceFixture(t *testing.T) (*taskdir.Store, *Resource, ResourceRequest) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	request := ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: testID('a'), TaskID: testID('b'),
		RequestSHA256: testSHA('c'), SourceCwd: filepath.Join(t.TempDir(), "source"), Container: filepath.Join(t.TempDir(), "container"), BudgetNanos: int64(time.Minute),
	}
	resource, err := EstablishResource(t.Context(), store, request)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, resource, request
}

func closeResource(t *testing.T, resource *Resource) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}

func testID(character byte) string  { return strings.Repeat(string(testHex(character)), 32) }
func testSHA(character byte) string { return strings.Repeat(string(testHex(character)), 64) }

func testHex(character byte) byte {
	return "0123456789abcdef"[int(character)%16]
}

func subjectFor(character byte) Subject {
	return Subject{Kind: SubjectOperation, ID: testID(character), SHA256: testSHA(character)}
}

func testSource(t *testing.T) Source {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	return Source{
		Root: root, CommonDir: filepath.Join(root, ".git"), GitDir: filepath.Join(root, ".git"), RelativeCwd: ".",
		BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1",
	}
}
