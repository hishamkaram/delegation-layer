package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
)

// newCustodyTestPlacement reconstructs a legacy pool, without retaining its
// retired creation protocol in production just to prepare cleanup fixtures.
func newCustodyTestPlacement(t *testing.T) *task.LifetimeCustodyPlacement {
	t.Helper()
	root := filepath.Join(testsupportcore.CanonicalTempDir(t), "pool")
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(nonce[:])
	writeCustodyTestPool(t, root, id)
	return &task.LifetimeCustodyPlacement{Version: task.LifetimeCustodyPlacementVersion, PoolRoot: root, PoolID: id}
}

func writeCustodyTestPool(t *testing.T, root, id string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := []byte(`{"version":1,"pool_id":"` + id + `"}`)
	if err := os.WriteFile(filepath.Join(root, ".identity"), marker, 0o600); err != nil {
		t.Fatal(err)
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

// bindCustodyTestView reconstructs an old retained view. New workers never
// create one; only the canonical closed owner may remove this legacy state.
func bindCustodyTestView(t *testing.T, registration *task.LifetimeRegistrationRecord) {
	t.Helper()
	custody, err := recordedLifetimeCustody(registration)
	if err != nil || custody == nil {
		t.Fatalf("recorded custody = %v, %v", custody, err)
	}
	view := filepath.Join(custody.pool.Root(), "views", custody.namespace.String(), "delegation-layer-verified-fixture")
	if err := os.MkdirAll(view, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view, "entrypoint"), []byte("legacy captured bytes"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(view, 0o500); err != nil {
		t.Fatal(err)
	}
	releaseCustodyTestOwnerAtCleanup(t, registration)
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

func TestRecordedLifetimeCustodyAcceptsNewUnplacedOwners(t *testing.T) {
	placement := custodyContractPlacement("a")
	for _, kind := range []string{task.LifetimeOwnerTask, task.LifetimeOwnerInspection} {
		if custody, err := recordedLifetimeCustody(custodyTestRegistration(kind, nil)); err != nil || custody != nil {
			t.Fatalf("%s registration without placement: %v, %v; want no custody", kind, custody, err)
		}
		custody, err := recordedLifetimeCustody(custodyTestRegistration(kind, placement))
		if err != nil || custody == nil {
			t.Fatalf("%s legacy registration: custody %v, error %v", kind, custody, err)
		}
		if got, want := custody.namespace.String(), strings.Repeat("1", 32)+"-"+kind+"-"+strings.Repeat("2", 32); got != want {
			t.Fatalf("%s namespace = %q, want %q", kind, got, want)
		}
		if got := custody.pool.Root(); got != placement.PoolRoot {
			t.Fatalf("%s pool root = %q, want recorded %q", kind, got, placement.PoolRoot)
		}
	}
	custody, err := recordedLifetimeCustody(custodyTestRegistration(task.LifetimeOwnerWorkspace, nil))
	if err != nil || custody != nil {
		t.Fatalf("workspace registration: custody %v, error %v, want none", custody, err)
	}
	if _, err = recordedLifetimeCustody(custodyTestRegistration(task.LifetimeOwnerWorkspace, placement)); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("workspace registration with placement: %v, want evidence fault", err)
	}
	if _, err = recordedLifetimeCustody(custodyTestRegistration("other", placement)); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unknown owner kind: %v, want evidence fault", err)
	}
}

func TestLegacyCustodyViewRemainsUntilOwnerRelease(t *testing.T) {
	placement := newCustodyTestPlacement(t)
	registration := custodyTestRegistration(task.LifetimeOwnerTask, placement)
	if custodyTestPending(t, registration) {
		t.Fatal("fresh owner already has a witness")
	}
	bindCustodyTestView(t, registration)
	if !custodyTestPending(t, registration) {
		t.Fatal("legacy retained view has no owner witness")
	}
}

func TestLegacyCustodyReleaseRefusesReplacedPool(t *testing.T) {
	placement := newCustodyTestPlacement(t)
	registration := custodyTestRegistration(task.LifetimeOwnerTask, placement)
	custody, err := recordedLifetimeCustody(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(placement.PoolRoot); err != nil {
		t.Fatal(err)
	}
	writeCustodyTestPool(t, placement.PoolRoot, strings.Repeat("b", 64))
	if err := custody.pool.ReleaseOwner(t.Context(), custody.namespace); err == nil {
		t.Fatal("release accepted a pool whose identity was replaced")
	}
}

func TestOpenTaskLifetimeChildChecksUnplacedScopeBeforeHello(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	recordTamperedLifetimeChildRecords(t, td, time.Now().Add(time.Minute).UTC(), nil)
	parent, childFD := newLifetimeChildTestPair(t)
	assertLifetimeChildRejectsBeforeHello(t, parent, func() (*openedLifetimeChild, error) {
		return openTaskLifetimeChild(runnerArguments{TaskID: request.TaskID, LifetimeChild: true, LifetimeFD: childFD}, td)
	}, lifetime.ErrInvalidRef)
}

func TestOpenInspectionLifetimeChildChecksUnplacedScopeBeforeHello(t *testing.T) {
	operation := newLifetimeChildTestInspection(t)
	recordTamperedLifetimeChildRecords(t, operation, operation.Deadline(), nil)
	parent, childFD := newLifetimeChildTestPair(t)
	assertLifetimeChildRejectsBeforeHello(t, parent, func() (*openedLifetimeChild, error) {
		return openInspectionLifetimeChild(runnerArguments{TaskID: operation.Request().TaskID, LifetimeChild: true, LifetimeFD: childFD}, operation)
	}, lifetime.ErrInvalidRef)
}

// lifetimeChildTestRecords is the record writer shared by the task and
// inspection journal families.
type lifetimeChildTestRecords interface {
	RecordLifetimeIntent(taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error)
	RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error)
}

// recordTamperedLifetimeChildRecords writes valid records whose scope does not
// match its plan. Custody placement is optional, but the scope must still pass
// lifetime validation before the worker sends Hello.
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
