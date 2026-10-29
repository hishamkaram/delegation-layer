package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	guardianfixture "github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// custodyTestExecutable is a small native executable present on Linux and macOS.
const custodyTestExecutable = "/usr/bin/true"

// newCustodyTestPlacement prepares a real pool and returns its placement.
func newCustodyTestPlacement(t *testing.T) *task.LifetimeCustodyPlacement {
	t.Helper()
	root := filepath.Join(testsupportcore.CanonicalTempDir(t), "pool")
	id, err := prepareLifetimeCustodyPool(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	return &task.LifetimeCustodyPlacement{Version: task.LifetimeCustodyPlacementVersion, PoolRoot: root, PoolID: id}
}

// custodyTestRequest admits a private copy of a native executable, so a view
// mirrors only that one directory entry.
func custodyTestRequest(t *testing.T) verifiedexec.CommandRequest {
	t.Helper()
	contents, err := os.ReadFile(custodyTestExecutable)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(testsupportcore.CanonicalTempDir(t), "bin", "tool")
	if err = os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(executable, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := verifiedexec.Fingerprint(executable)
	if err != nil {
		t.Fatal(err)
	}
	return verifiedexec.CommandRequest{
		Executable: executable, SHA256: digest, Directory: filepath.Dir(executable),
		Environment: []string{"PATH=/usr/bin:/bin"},
	}
}

// releaseCustodyTestOwnerAtCleanup removes the sealed views a test left behind,
// before the temporary directory is removed.
func releaseCustodyTestOwnerAtCleanup(t *testing.T, registration *task.LifetimeRegistrationRecord) {
	t.Helper()
	custody, err := recordedLifetimeCustody(registration)
	if err != nil || custody == nil {
		t.Fatalf("recorded custody = %v, %v", custody, err)
	}
	t.Cleanup(func() {
		if _, statErr := os.Lstat(custody.pool.Root()); errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if releaseErr := custody.pool.ReleaseOwner(context.Background(), custody.namespace); releaseErr != nil {
			t.Errorf("release test custody: %v", releaseErr)
		}
	})
}

func custodyTestRegistration(ownerKind string, placement *task.LifetimeCustodyPlacement) *task.LifetimeRegistrationRecord {
	return &task.LifetimeRegistrationRecord{
		RootID: strings.Repeat("1", 32), TaskID: strings.Repeat("2", 32), OwnerKind: ownerKind,
		CustodyPlacement: placement.Clone(),
	}
}

// bindCustodyTestView binds one view for registration's owner, as the worker
// does, and closes only the command's local handles.
func bindCustodyTestView(t *testing.T, registration *task.LifetimeRegistrationRecord) {
	t.Helper()
	binder, err := lifetimeChildBinder(registration, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := custodyTestRequest(t)
	releaseCustodyTestOwnerAtCleanup(t, registration)
	command, err := binder.BindCommand(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Close(); err != nil {
		t.Fatal(err)
	}
}

// moveCustodyTestPool moves the recorded pool away, so its root is missing,
// and returns the restore step; cleanup restores it if the test did not.
func moveCustodyTestPool(t *testing.T, placement *task.LifetimeCustodyPlacement) func() {
	t.Helper()
	moved := placement.PoolRoot + ".moved"
	if err := os.Rename(placement.PoolRoot, moved); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := os.Rename(moved, placement.PoolRoot); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// custodyTestPending observes the recorded owner's witness read-only.
func custodyTestPending(t *testing.T, registration *task.LifetimeRegistrationRecord) bool {
	t.Helper()
	custody, err := recordedLifetimeCustody(registration)
	if err != nil || custody == nil {
		t.Fatalf("recorded custody = %v, %v", custody, err)
	}
	pending, err := custody.pool.ReleasePending(custody.namespace)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

func TestLifetimeChildBinderRequiresPlacementByOwnerKind(t *testing.T) {
	placement := custodyContractPlacement("a")
	deadline := time.Now().Add(time.Minute)
	for _, kind := range []string{task.LifetimeOwnerTask, task.LifetimeOwnerInspection} {
		if _, err := lifetimeChildBinder(custodyTestRegistration(kind, nil), deadline); !errors.Is(err, errLifetimeCustodyUnplaced) || !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("%s registration without placement: %v, want refusal", kind, err)
		}
		binder, err := lifetimeChildBinder(custodyTestRegistration(kind, placement), deadline)
		if err != nil || binder == nil {
			t.Fatalf("%s placed registration: binder %v, error %v", kind, binder, err)
		}
		if got, want := binder.custody.namespace.String(), strings.Repeat("1", 32)+"-"+kind+"-"+strings.Repeat("2", 32); got != want {
			t.Fatalf("%s namespace = %q, want %q", kind, got, want)
		}
		if got := binder.custody.pool.Root(); got != placement.PoolRoot {
			t.Fatalf("%s pool root = %q, want recorded %q", kind, got, placement.PoolRoot)
		}
	}
	binder, err := lifetimeChildBinder(custodyTestRegistration(task.LifetimeOwnerWorkspace, nil), deadline)
	if err != nil || binder != nil {
		t.Fatalf("workspace registration: binder %v, error %v, want none", binder, err)
	}
	if _, err = lifetimeChildBinder(custodyTestRegistration(task.LifetimeOwnerWorkspace, placement), deadline); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("workspace registration with placement: %v, want evidence fault", err)
	}
	if _, err = lifetimeChildBinder(custodyTestRegistration("other", placement), deadline); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unknown owner kind: %v, want evidence fault", err)
	}
}

func TestOpenedLifetimeChildBinderIsUntypedNilWithoutCustody(t *testing.T) {
	var absent *openedLifetimeChild
	if absent.Binder() != nil {
		t.Fatal("nil lifetime child returned a binder; execution would skip its private copy")
	}
	if (&openedLifetimeChild{}).Binder() != nil {
		t.Fatal("workspace lifetime child returned a typed-nil binder")
	}
}

func TestLifetimeCustodyBinderBindsRecordedPoolWithinRegistrationDeadline(t *testing.T) {
	placement := newCustodyTestPlacement(t)
	registration := custodyTestRegistration(task.LifetimeOwnerTask, placement)
	if custodyTestPending(t, registration) {
		t.Fatal("fresh owner already has a witness")
	}
	// The initial provider binding receives context.Background.
	bindCustodyTestView(t, registration)
	if !custodyTestPending(t, registration) {
		t.Fatal("binding did not leave its view in the recorded pool")
	}

	expired, err := lifetimeChildBinder(custodyTestRegistration(task.LifetimeOwnerInspection, placement), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = expired.BindCommand(context.Background(), custodyTestRequest(t)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bind after the registration deadline: %v, want deadline exceeded", err)
	}
	live, err := lifetimeChildBinder(custodyTestRegistration(task.LifetimeOwnerInspection, placement), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = live.BindCommand(canceled, custodyTestRequest(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("bind with a canceled caller: %v, want the caller's cancellation", err)
	}
	if custodyTestPending(t, custodyTestRegistration(task.LifetimeOwnerInspection, placement)) {
		t.Fatal("a refused bind created the owner's witness")
	}
}

func TestLifetimeCustodyBinderRefusesReplacedPool(t *testing.T) {
	placement := newCustodyTestPlacement(t)
	if err := os.RemoveAll(placement.PoolRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLifetimeCustodyPool(t.Context(), placement.PoolRoot); err != nil {
		t.Fatal(err)
	}
	binder, err := lifetimeChildBinder(custodyTestRegistration(task.LifetimeOwnerTask, placement), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := custodyTestRequest(t)
	if command, bindErr := binder.BindCommand(t.Context(), request); bindErr == nil {
		t.Fatalf("bind into a replaced pool succeeded: %v", errors.Join(command.Close()))
	}
}

func TestOpenTaskLifetimeChildRefusesUnplacedRegistrationBeforeHello(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	recordTamperedLifetimeChildRecords(t, td, time.Now().Add(time.Minute).UTC(), nil)
	parent, childFD := newLifetimeChildTestPair(t)
	assertLifetimeChildRejectsBeforeHello(t, parent, func() (*openedLifetimeChild, error) {
		return openTaskLifetimeChild(runnerArguments{TaskID: request.TaskID, LifetimeChild: true, LifetimeFD: childFD}, td)
	}, errLifetimeCustodyUnplaced)
}

func TestOpenInspectionLifetimeChildRefusesUnplacedRegistrationBeforeHello(t *testing.T) {
	operation := newLifetimeChildTestInspection(t)
	recordTamperedLifetimeChildRecords(t, operation, operation.Deadline(), nil)
	parent, childFD := newLifetimeChildTestPair(t)
	assertLifetimeChildRejectsBeforeHello(t, parent, func() (*openedLifetimeChild, error) {
		return openInspectionLifetimeChild(runnerArguments{TaskID: operation.Request().TaskID, LifetimeChild: true, LifetimeFD: childFD}, operation)
	}, errLifetimeCustodyUnplaced)
}

// lifetimeChildTestRecords is the record writer shared by the task and
// inspection journal families.
type lifetimeChildTestRecords interface {
	RecordLifetimeIntent(taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error)
	RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error)
}

// recordTamperedLifetimeChildRecords writes valid records whose scope does not
// match its plan, so a child that opened the scope would fail with
// lifetime.ErrInvalidRef rather than with a custody refusal.
func recordTamperedLifetimeChildRecords(t *testing.T, records lifetimeChildTestRecords, deadline time.Time, placement *task.LifetimeCustodyPlacement) {
	t.Helper()
	bootID, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = records.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("a", 32), BootID: bootID, Deadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = records.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopeID: "pg:" + strings.Repeat("b", 32), BootID: bootID, Deadline: deadline,
		CustodyPlacement: placement,
	}); err != nil {
		t.Fatal(err)
	}
}

func newLifetimeChildTestPair(t *testing.T) (net.Conn, int) {
	t.Helper()
	parent, childFile, err := newLifetimeControlPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { guardianfixture.CloseConnection(t, parent) })
	t.Cleanup(func() {
		if closeErr := childFile.Close(); closeErr != nil {
			t.Errorf("close child file: %v", closeErr)
		}
	})
	return parent, int(childFile.Fd())
}

func newLifetimeChildTestInspection(t *testing.T) *inspection.Operation {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		Supervisor: task.SupervisorRef{
			ClientExecutable: "/usr/bin/true", ClientSHA256: digest, ResolvedConfigSHA256: digest,
			Endpoint: "/private/pueue.sock", ConfigPath: filepath.Join(request.CanonicalCwd, "pueue.yml"),
			ConfigDigest: digest, ObservedVersion: pueue.FixtureVersion,
		},
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation
}
