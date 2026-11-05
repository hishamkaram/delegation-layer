package control

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type migrationTargetResolverFunc func(context.Context, string, PrivateMigrationIdentity) (task.SupervisorRef, error)

func (f migrationTargetResolverFunc) ResolvePrivateMigrationTarget(ctx context.Context, root string, identity PrivateMigrationIdentity) (task.SupervisorRef, error) {
	return f(ctx, root, identity)
}

type referencedMigrationFixture struct {
	root    string
	base    string
	target  task.SupervisorRef
	record  privateMigrationRecord
	options Options
}

// These are protocol fixtures using production private stores and finite shell
// client/daemon fixtures. The resolver fake proves no managed-pair authority.
func newReferencedMigrationFixture(t *testing.T) referencedMigrationFixture {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	target := current.Binding()
	legacy := privateDaemonIdentityForBinding(target)
	legacy.Executable = filepath.Join(filepath.Dir(fixture.daemonPath), "old-pueued")
	legacy.SHA256 = task.ComputeSHA256([]byte("old daemon fixture"))
	return referencedMigrationFixture{fixture.stateRoot, filepath.Dir(target.ConfigPath), target, privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: legacy, Successor: privateDaemonIdentityForBinding(target)}, options}
}

func (f referencedMigrationFixture) resolver(t *testing.T) PrivateMigrationTargetResolver {
	t.Helper()
	return migrationTargetResolverFunc(func(_ context.Context, root string, identity PrivateMigrationIdentity) (task.SupervisorRef, error) {
		if root != f.root || identity != f.record.Successor {
			t.Fatalf("resolver received unvalidated subject: %q %+v", root, identity)
		}
		return f.target, nil
	})
}

func completedReferencedRecord(record privateMigrationRecord, pid int) privateMigrationRecord {
	record.LegacyPID = pid
	record.LegacyStopped = true
	return record
}

func captureReferencedMigration(t *testing.T, f referencedMigrationFixture) PrivateMigrationReference {
	t.Helper()
	ref, found, err := ReadPrivateMigrationReferenceWithLock(f.root)
	if err != nil || !found {
		t.Fatalf("capture pending: found=%v err=%v", found, err)
	}
	return ref
}

func TestReferencedMigrationFirstProjectionSurvivesAppendAndDedup(t *testing.T) {
	f := newReferencedMigrationFixture(t)
	if err := publishPrivateMigrationState(f.base, f.record); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	if ref.HistoryIndex != 0 {
		t.Fatalf("initial index = %d", ref.HistoryIndex)
	}
	completed := completedReferencedRecord(f.record, 12345)
	for _, record := range []privateMigrationRecord{completed, completed, completedReferencedRecord(f.record, 12346)} {
		if err := appendPrivateMigrationHistory(f.base, record); err != nil {
			t.Fatal(err)
		}
	}
	history, _, err := loadPrivateMigrationHistory(f.base)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Migrations) != 2 {
		t.Fatalf("ordinary full-record dedup changed: %d records", len(history.Migrations))
	}
	if after := captureReferencedMigration(t, f); after != ref {
		t.Fatalf("reference moved after append: before=%+v after=%+v", ref, after)
	}
	if f.record.LegacyPID != 0 {
		t.Fatal("prepared fixture must have no process proof")
	}
	if err = appendPrivateMigrationHistory(f.base, f.record); err == nil {
		t.Fatal("PID-0 prepared record was accepted as completed history")
	}
	bad := ref
	bad.HistoryIndex = 1
	if _, _, err = referencedPrivateMigrationRecord(f.root, bad); err == nil {
		t.Fatal("later matching slot accepted instead of first")
	}
}

func TestReferencedMigrationAppendBeforeClearAndLostReply(t *testing.T) {
	f := newReferencedMigrationFixture(t)
	pending := completedReferencedRecord(f.record, 12345)
	if err := publishPrivateMigrationState(f.base, pending); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	if err := appendPrivateMigrationHistory(f.base, pending); err != nil {
		t.Fatal(err)
	}
	if after := captureReferencedMigration(t, f); after != ref {
		t.Fatalf("append-before-clear rederive changed reference: %+v", after)
	}
	result, err := ExecuteReferencedPrivateMigrationWithLock(context.Background(), f.root, ref, f.resolver(t), f.options)
	if err != nil || result != PrivateMigrationCompleted {
		t.Fatalf("finalize appended pending: result=%s err=%v", result, err)
	}
	if _, found, readErr := loadPrivateMigrationRecord(f.base); readErr != nil || found {
		t.Fatalf("canonical journal not cleared: found=%v err=%v", found, readErr)
	}
	result, err = ExecuteReferencedPrivateMigrationWithLock(context.Background(), f.root, ref, f.resolver(t), f.options)
	if err != nil || result != PrivateMigrationWarmReuse {
		t.Fatalf("lost reply replay: result=%s err=%v", result, err)
	}
	assertPrivateDaemonStartCount(t, f.root, 1)
}

func TestReferencedMigrationRejectsReferenceBeforeResolver(t *testing.T) {
	for _, kind := range []string{"root", "body", "slot", "negative-slot", "slot-conflict", "pending-z", "missing-history"} {
		t.Run(kind, func(t *testing.T) { testReferencedMigrationBadReference(t, kind) })
	}
}

func testReferencedMigrationBadReference(t *testing.T, kind string) {
	t.Helper()
	f := newReferencedMigrationFixture(t)
	if err := publishPrivateMigrationState(f.base, f.record); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	ref = invalidateReferencedMigration(t, f, ref, kind)
	treeBefore := snapshotPrivateSuccessorTestState(t, f.root)
	before, readErr := os.ReadFile(privateMigrationRecordPath(f.base))
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	called := false
	resolver := migrationTargetResolverFunc(func(context.Context, string, PrivateMigrationIdentity) (task.SupervisorRef, error) {
		called = true
		return f.target, nil
	})
	if _, err := ExecuteReferencedPrivateMigrationWithLock(context.Background(), f.root, ref, resolver, f.options); err == nil {
		t.Fatal("invalid reference accepted")
	}
	if called {
		t.Fatal("resolver invoked before exact reference validation")
	}
	after, afterErr := os.ReadFile(privateMigrationRecordPath(f.base))
	if string(before) != string(after) || errors.Is(readErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) {
		t.Fatal("refusal mutated pending journal")
	}
	if !maps.Equal(treeBefore, snapshotPrivateSuccessorTestState(t, f.root)) {
		t.Fatal("reference refusal mutated root filesystem")
	}
	assertPrivateDaemonStartCount(t, f.root, 1)
}

func TestReferencedMigrationWarmReuseRequiresFullCurrentReadyTarget(t *testing.T) {
	for _, kind := range []string{"warm-different-context", "old-history-current-x", "foreign-client", "not-ready"} {
		t.Run(kind, func(t *testing.T) { testReferencedMigrationCurrentTarget(t, kind) })
	}
}

func testReferencedMigrationCurrentTarget(t *testing.T, kind string) {
	t.Helper()
	f := newReferencedMigrationFixture(t)
	if err := publishPrivateMigrationState(f.base, f.record); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	if err := appendPrivateMigrationHistory(f.base, completedReferencedRecord(f.record, 12345)); err != nil {
		t.Fatal(err)
	}
	if err := clearPrivateMigrationRecord(f.base, f.record); err != nil {
		t.Fatal(err)
	}
	setReferencedMigrationCurrentTarget(t, f, kind)
	result, err := ExecuteReferencedPrivateMigrationWithLock(context.Background(), f.root, ref, f.resolver(t), f.options)
	switch kind {
	case "warm-different-context":
		if err != nil || result != PrivateMigrationWarmReuse {
			t.Fatalf("warm context reuse: %s %v", result, err)
		}
	case "not-ready":
		if !errors.Is(err, ErrReferencedMigrationTargetNotReady) {
			t.Fatalf("missing distinct not-ready cause: %v", err)
		}
	default:
		if err == nil {
			t.Fatalf("%s incorrectly completed: %s", kind, result)
		}
	}
	assertPrivateDaemonStartCount(t, f.root, 1)
}

func TestReferencedMigrationUnsupportedAndMismatchedTargetPreserveJournal(t *testing.T) {
	for _, kind := range []string{"unsupported", "wrong-daemon", "wrong-logical", "missing-client", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			testReferencedMigrationUnsupportedTarget(t, kind)
		})
	}
}

func TestReferencedMigrationStoppedRestartRequiresGuardAndRegistersWait(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		name := "missing-guard"
		if guarded {
			name = "guarded-wait"
		}
		t.Run(name, func(t *testing.T) {
			testReferencedMigrationStoppedRestart(t, guarded)
		})
	}
}

func TestReferencedMigrationResumesRecordedTargetAcrossShutdownPhases(t *testing.T) {
	for _, phase := range []string{"prepared", "requested", "dispatched", "stopped"} {
		t.Run(phase, func(t *testing.T) { testReferencedMigrationShutdownPhase(t, phase) })
	}
}

// The existing observer models the finite shell daemon's PID-file lifecycle;
// this is a canonical journal/phase test, not native incarnation evidence.
func testReferencedMigrationShutdownPhase(t *testing.T, phase string) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	next := filepath.Join(filepath.Dir(fixture.daemonPath), "recorded-y-pueued")
	writeExecutable(t, next, privateDaemonFixture+"\n# recorded target Y\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, next, options)
	target := current.Binding()
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(legacy.Binding()), Successor: privateDaemonIdentityForBinding(target)}
	pending = referencedMigrationPhaseRecord(t, legacy, pending, phase)
	base := filepath.Dir(target.ConfigPath)
	if err := publishPrivateMigrationState(base, pending); err != nil {
		t.Fatal(err)
	}
	if phase == "dispatched" || phase == "stopped" {
		if err := legacy.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ref, found, err := ReadPrivateMigrationReferenceWithLock(fixture.stateRoot)
	if err != nil || !found {
		t.Fatalf("capture: found=%v err=%v", found, err)
	}
	resolver := migrationTargetResolverFunc(func(_ context.Context, _ string, successor PrivateMigrationIdentity) (task.SupervisorRef, error) {
		if successor != pending.Successor {
			t.Fatal("resolver did not receive recorded Y")
		}
		return target, nil
	})
	err = WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		result, executeErr := ExecuteReferencedPrivateMigrationWithLock(ctx, fixture.stateRoot, ref, resolver, options)
		if executeErr == nil && result != PrivateMigrationCompleted {
			t.Errorf("phase %s returned %s", phase, result)
		}
		return executeErr
	})
	if err != nil {
		t.Fatalf("resume recorded phase %s: %v", phase, err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, next)
	if _, found, err = loadPrivateMigrationRecord(base); err != nil || found {
		t.Fatalf("phase retained journal: found=%v err=%v", found, err)
	}
}

func TestReferencedMigrationHistoricalTargetRefusesBeforeResolver(t *testing.T) {
	f := newReferencedMigrationFixture(t)
	f.record.Successor.ConfigPath = ""
	f.record.Successor.ConfigDigest = ""
	f.record.Successor.ResolvedConfigSHA256 = ""
	f.record.Successor.Endpoint = ""
	if err := publishPrivateMigrationState(f.base, f.record); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	before := snapshotPrivateSuccessorTestState(t, f.root)
	called := false
	resolver := migrationTargetResolverFunc(func(context.Context, string, PrivateMigrationIdentity) (task.SupervisorRef, error) {
		called = true
		return f.target, nil
	})
	_, err := ExecuteReferencedPrivateMigrationWithLock(context.Background(), f.root, ref, resolver, f.options)
	if !errors.Is(err, ErrReferencedMigrationUnsupported) || called {
		t.Fatalf("historical target: resolverCalled=%v err=%v", called, err)
	}
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, f.root)) {
		t.Fatal("historical refusal mutated root filesystem")
	}
}

func invalidateReferencedMigration(t *testing.T, f referencedMigrationFixture, ref PrivateMigrationReference, kind string) PrivateMigrationReference {
	t.Helper()
	switch kind {
	case "root":
		ref.RootID = strings.Repeat("a", 32)
	case "body":
		ref.RecordSHA256 = strings.Repeat("a", 64)
	case "slot":
		ref.HistoryIndex++
	case "negative-slot":
		ref.HistoryIndex = -1
	case "slot-conflict":
		other := completedReferencedRecord(f.record, 12345)
		other.Successor.SHA256 = task.ComputeSHA256([]byte("conflicting history slot"))
		if err := appendPrivateMigrationHistory(f.base, other); err != nil {
			t.Fatal(err)
		}
	case "pending-z":
		if err := appendPrivateMigrationHistory(f.base, completedReferencedRecord(f.record, 12345)); err != nil {
			t.Fatal(err)
		}
		other := f.record
		other.Successor.SHA256 = task.ComputeSHA256([]byte("different pending target"))
		if err := replacePrivateMigrationRecord(f.base, f.record, other); err != nil {
			t.Fatal(err)
		}
	case "missing-history":
		if err := clearPrivateMigrationRecord(f.base, f.record); err != nil {
			t.Fatal(err)
		}
	}
	return ref
}

func testReferencedMigrationUnsupportedTarget(t *testing.T, kind string) {
	t.Helper()
	f := newReferencedMigrationFixture(t)
	if err := publishPrivateMigrationState(f.base, f.record); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	before, err := os.ReadFile(privateMigrationRecordPath(f.base))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if kind == "canceled" {
		cancel()
	}
	resolver := migrationTargetResolverFunc(func(context.Context, string, PrivateMigrationIdentity) (task.SupervisorRef, error) {
		target := f.target
		switch kind {
		case "unsupported":
			return task.SupervisorRef{}, ErrReferencedMigrationUnsupported
		case "wrong-daemon":
			target.DaemonSHA256 = f.record.Legacy.SHA256
		case "wrong-logical":
			target.ConfigDigest = task.ComputeSHA256([]byte("foreign logical config"))
		case "missing-client":
			target.ClientExecutable = filepath.Join(f.base, "missing-client")
		}
		return target, nil
	})
	if _, err = ExecuteReferencedPrivateMigrationWithLock(ctx, f.root, ref, resolver, f.options); err == nil {
		t.Fatal("invalid target accepted")
	}
	after, err := os.ReadFile(privateMigrationRecordPath(f.base))
	if err != nil || string(before) != string(after) {
		t.Fatalf("invalid target mutated journal: %v", err)
	}
	assertPrivateDaemonStartCount(t, f.root, 1)
}

func testReferencedMigrationStoppedRestart(t *testing.T, guarded bool) {
	t.Helper()
	f := newReferencedMigrationFixture(t)
	stopped := completedReferencedRecord(f.record, 12345)
	if err := publishPrivateMigrationState(f.base, stopped); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	if err := os.Remove(filepath.Join(f.base, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, f.root)
	waits := make(chan (<-chan struct{}), 1)
	f.options.DaemonWaitOwner = daemonWaitOwnerFunc(func(root string, pid int, done <-chan struct{}) error {
		if root != f.root || pid <= 0 {
			t.Errorf("wrong wait registration root=%q pid=%d", root, pid)
		}
		waits <- done
		return nil
	})
	if !guarded {
		f.options.DaemonStartGuard = nil
	}
	result, err := ExecuteReferencedPrivateMigrationWithLock(context.Background(), f.root, ref, f.resolver(t), f.options)
	if !guarded {
		if !errors.Is(err, ErrInfrastructureOwnerRequired) {
			t.Fatalf("unguarded start accepted: %s %v", result, err)
		}
		if len(waits) != 0 {
			t.Fatal("failed Start registered a wait")
		}
		assertPrivateDaemonStartCount(t, f.root, 1)
		return
	}
	if err != nil || result != PrivateMigrationCompleted {
		t.Fatalf("guarded completion: %s %v", result, err)
	}
	select {
	case done := <-waits:
		<-done
	default:
		t.Fatal("native Start lacks wait ownership")
	}
	assertPrivateDaemonStartCount(t, f.root, 2)
}

func setReferencedMigrationCurrentTarget(t *testing.T, f referencedMigrationFixture, kind string) {
	t.Helper()
	binding := f.target
	switch kind {
	case "warm-different-context":
		binding.ResolutionHome = "/previous-selected-context"
	case "old-history-current-x":
		binding.DaemonSHA256 = f.record.Legacy.SHA256
		binding.DaemonExecutable = f.record.Legacy.Executable
	case "foreign-client":
		binding.ClientSHA256 = task.ComputeSHA256([]byte("foreign client"))
	case "not-ready":
		if err := os.Remove(filepath.Join(f.base, "ready")); err != nil {
			t.Fatal(err)
		}
		removePrivateTestPID(t, f.root)
	}
	if err := publishPrivateDaemonIdentity(f.base, binding); err != nil {
		t.Fatal(err)
	}
}

func referencedMigrationPhaseRecord(t *testing.T, legacy *Client, pending privateMigrationRecord, phase string) privateMigrationRecord {
	t.Helper()
	if phase != "prepared" {
		process, present, err := legacy.privateDaemonProcessIdentity()
		if err != nil || !present {
			t.Fatalf("fixture process: present=%v err=%v", present, err)
		}
		pending.LegacyPID, pending.LegacyProcessStart = process.PID, process.StartToken
		pending.LegacyShutdownRequested = true
		pending.LegacyShutdownDispatched = phase == "dispatched" || phase == "stopped"
		pending.LegacyStopped = phase == "stopped"
	}
	return pending
}

func TestReferencedMigrationPhaseMismatchRefusesWithoutOrdinaryControls(t *testing.T) {
	f := newReferencedMigrationFixture(t)
	pending := completedReferencedRecord(f.record, 12345)
	if err := publishPrivateMigrationState(f.base, pending); err != nil {
		t.Fatal(err)
	}
	z := filepath.Join(filepath.Dir(f.target.DaemonExecutable), "different-z-pueued")
	writeExecutable(t, z, privateDaemonFixture+"\n# injected different target Z\n")
	selected := f.target
	selected.DaemonExecutable, selected.DaemonSHA256 = z, task.ComputeSHA256([]byte(privateDaemonFixture+"\n# injected different target Z\n"))
	var entries atomic.Int32
	options := f.options
	options.DaemonStartGuard = nil
	options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" {
			entries.Add(1)
		}
	}
	current, err := NewClient(selected, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := current.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err = current.resumeReferencedPrivateMigrationPhase(t.Context(), f.root, f.target, true, privateDaemonIdentityForBinding(f.target), true, pending)
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, f.root)) || entries.Load() != 0 {
		t.Fatal("referenced target mismatch entered ordinary migration controls or wrote state")
	}
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("injected target mismatch lost explicit refusal: %v", err)
	}
}

// This injected prepared/current-successor state is not a canonical writer
// postimage. It must refuse proof without repair; supported ordinary recovery
// is tested separately against a canonically published Legacy below.
func TestReferencedMigrationPIDZeroCurrentSuccessorRefusesReadOnly(t *testing.T) {
	f := newReferencedMigrationFixture(t)
	if err := publishPrivateMigrationState(f.base, f.record); err != nil {
		t.Fatal(err)
	}
	ref := captureReferencedMigration(t, f)
	before := snapshotPrivateSuccessorTestState(t, f.root)
	_, err := ExecuteReferencedPrivateMigrationWithLock(t.Context(), f.root, ref, f.resolver(t), f.options)
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, f.root)) {
		t.Fatal("PID-0 proof refusal wrote state")
	}
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("prepared current-successor fixture invented closure: %v", err)
	}
}

func TestReferencedMigrationPIDZeroCanonicalLegacyWriterRecovers(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	pending, found, err := loadPrivateMigrationRecord(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	if err != nil || !found || pending.LegacyPID != 0 || pending.LegacyStopped {
		t.Fatalf("canonical busy fixture must have intent without shutdown proof: found=%t err=%v", found, err)
	}
	// The canonical writer still publishes Legacy and its matching native
	// identity. Explicit ordinary selection may withdraw this pre-stop intent;
	// it must not invent completion of the referenced successor.
	writePreparedPrivateQueue(t, fixture, false)
	if err = migratePreparedPrivateWriter(t, fixture, fixture.legacy); err != nil {
		t.Fatalf("ordinary explicit Legacy migration failed to recover prepared intent: %v", err)
	}
	assertPreparedPrivateIntentAbsent(t, fixture)
	assertPreparedPrivatePublishedLegacy(t, fixture)
	assertPreparedPrivateNoHistory(t, fixture)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}
