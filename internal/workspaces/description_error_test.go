package workspaces

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

// Store records whose durable link precedes an injected fault.
const (
	unlaunchedFenceRecord    = "lifetime-unlaunched.json"
	checkpointReceiptRecord  = "checkpoint-receipt.json"
	genericDescribedHelperID = "5555555555555555555555555555555a"
)

var (
	errInjectedRecordFault = errors.New("injected record fault")
	errOwnerUnlock         = errors.New("operation lifetime owner unlock failed")
)

// ownerUnlockFault fails the first unlock of one operation's lifetime-owner
// lock. Recovering an unfinished helper without lifetime intent then returns
// the production error: ErrPending joined with this release fault by its
// deferred owner release. The failed unlock keeps the lock, so later calls in
// this process observe a busy owner, which is pure pending.
type ownerUnlockFault struct {
	taskdir.BaseFaultInjector
	path  string
	fired bool
}

func (f *ownerUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (f *ownerUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *ownerUnlockFault) OnFlock(path string, operation int) error {
	if path == f.path && operation == unix.LOCK_UN && !f.fired {
		f.fired = true
		return errOwnerUnlock
	}
	return nil
}

func injectOwnerUnlockFault(t *testing.T, store *taskdir.Store, operationID string) *ownerUnlockFault {
	t.Helper()
	fault := &ownerUnlockFault{path: workspacefixture.OperationDirectory(store, operationID) + "/.lifetime-owner.lock"}
	setStoreFault(t, store, fault)
	return fault
}

// requirePendingJoinedFault asserts the whole production error: the pending
// marker joined with the fired release fault, which keeps it non-pending.
func requirePendingJoinedFault(t *testing.T, label string, err error, fault *ownerUnlockFault) {
	t.Helper()
	if !fault.fired || !errors.Is(err, workspace.ErrPending) || !errors.Is(err, errOwnerUnlock) || PendingError(err) {
		t.Fatalf("%s = %v (fired %t); want pending joined with the release fault", label, err, fault.fired)
	}
}

func closeOperation(t *testing.T, operation *workspace.Operation) {
	t.Helper()
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
}

// persistUnfinishedVerification persists an unexpired verification helper
// without lifetime intent.
func persistUnfinishedVerification(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	closeOperation(t, openWorkspaceHelper(t, fixture, genericDescribedHelperID, workspace.OperationVerify, verificationSubject(genericDescribedHelperID), time.Now().Add(time.Hour)))
}

// persistUnfinishedProvision persists an unexpired provision helper without
// lifetime intent.
func persistUnfinishedProvision(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	closeOperation(t, openWorkspaceHelper(t, fixture, genericDescribedHelperID, workspace.OperationProvision, verificationSubject(genericDescribedHelperID), time.Now().Add(time.Hour)))
}

func persistManagedCheckpoint(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	closeOperation(t, newReservedManagedCheckpointIn(t, fixture).operation)
}

func setStoreFault(t *testing.T, store *taskdir.Store, fault taskdir.FaultInjector) {
	t.Helper()
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
}

// openWorkspaceHelper opens a helper of the fixture workspace bound to
// subject. Without lifetime intent it is pending until its deadline, then
// its closure seals the unlaunched fence.
func openWorkspaceHelper(t *testing.T, fixture *workspacefixture.RecoveryFixture, operationID, kind string, subject workspace.Subject, deadline time.Time) *workspace.Operation {
	t.Helper()
	request := workspacefixture.RecoveryOperationRequest(t, fixture, operationID, kind, subject)
	request.Deadline = deadline.UTC().Format(time.RFC3339Nano)
	if kind == workspace.OperationVerify {
		request.Arguments = []string{"/usr/bin/true"}
	}
	return workspacefixture.OpenSelectionOperation(t, fixture, request)
}

func verificationSubject(operationID string) workspace.Subject {
	return workspace.Subject{Kind: workspace.SubjectOperation, ID: operationID, SHA256: task.ComputeSHA256([]byte("verification " + operationID))}
}

func requireDescribedPending(t *testing.T, err error, response Description) {
	t.Helper()
	if err != nil || response.NextAction != "watch_workspace" {
		t.Fatalf("described pending operation = %v, next %q; want watch guidance", err, response.NextAction)
	}
}

func requireNoWatchGuidance(t *testing.T, response Description) {
	t.Helper()
	if response.NextAction != "" {
		t.Fatalf("faulted description kept watch guidance %q", response.NextAction)
	}
}

func describeFixtureOperation(t *testing.T, fixture *workspacefixture.RecoveryFixture, operationID string) (Description, error) {
	t.Helper()
	var response Description
	err := DescribeOperation(t.Context(), fixture.Store, fixture.Resource, operationID, &response)
	return response, err
}

func TestDescribeOperationKeepsGenericFaultJoinedWithPending(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistUnfinishedVerification, genericDescribedHelperID)
	if fixture == nil {
		return
	}
	fault := injectOwnerUnlockFault(t, fixture.Store, genericDescribedHelperID)
	response, err := describeFixtureOperation(t, fixture, genericDescribedHelperID)
	requirePendingJoinedFault(t, "described operation", err, fault)
	requireNoWatchGuidance(t, response)
	// The failed release retained the owner: a later description is busy.
	response, err = describeFixtureOperation(t, fixture, genericDescribedHelperID)
	requireDescribedPending(t, err, response)
}

func TestDescribeOperationGenericControls(t *testing.T) {
	t.Run("pending only", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		openWorkspaceHelper(t, fixture, genericDescribedHelperID, workspace.OperationVerify, verificationSubject(genericDescribedHelperID), time.Now().Add(time.Hour))
		response, err := describeFixtureOperation(t, fixture, genericDescribedHelperID)
		requireDescribedPending(t, err, response)
	})
	t.Run("bare fence fault", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		openWorkspaceHelper(t, fixture, genericDescribedHelperID, workspace.OperationVerify, verificationSubject(genericDescribedHelperID), time.Now().Add(-time.Second))
		setStoreFault(t, fixture.Store, &linkedRecordFault{name: unlaunchedFenceRecord, err: errInjectedRecordFault})
		response, err := describeFixtureOperation(t, fixture, genericDescribedHelperID)
		if !errors.Is(err, errInjectedRecordFault) || PendingError(err) {
			t.Fatalf("fence fault = %v, want it whole", err)
		}
		requireNoWatchGuidance(t, response)
	})
}

func TestDescribeOperationKeepsCheckpointFaultJoinedWithPending(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistManagedCheckpoint, reservedManagedCheckpointID)
	if fixture == nil {
		return
	}
	fault := injectOwnerUnlockFault(t, fixture.Store, reservedManagedCheckpointID)
	response, err := describeFixtureOperation(t, fixture, reservedManagedCheckpointID)
	requirePendingJoinedFault(t, "described checkpoint", err, fault)
	requireNoWatchGuidance(t, response)
}

func TestDescribeOperationCheckpointControls(t *testing.T) {
	cases := map[string]struct {
		closed bool
		fault  *linkedRecordFault
	}{
		"receipt cleanup fault": {true, &linkedRecordFault{name: checkpointReceiptRecord, err: errInjectedRecordFault}},
		"pending only":          {false, nil},
		"clean":                 {true, nil},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			checkpoint := newReservedManagedCheckpoint(t)
			fixture, operation := checkpoint.recovery, checkpoint.operation
			if test.closed {
				closeCheckpointLifetime(t, fixture.Store, operation, checkpointNative, func() { recordCheckpointWork(t, operation, &checkpoint.target) })
			}
			if test.fault != nil {
				setStoreFault(t, fixture.Store, test.fault)
			}
			response, err := describeFixtureOperation(t, fixture, operation.Request().OperationID)
			switch {
			case test.fault != nil:
				if !errors.Is(err, errInjectedRecordFault) || PendingError(err) {
					t.Fatalf("receipt cleanup fault = %v, want it whole", err)
				}
				requireNoWatchGuidance(t, response)
			case !test.closed:
				requireDescribedPending(t, err, response)
			case err != nil || response.NextAction != "" || response.ReasonCode != "" || response.Operation != nil:
				t.Fatalf("clean checkpoint description = %+v, %v", response, err)
			}
		})
	}
}
