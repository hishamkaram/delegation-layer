package infrastructurehost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type settledUpgradeCleanupError struct{ cause error }

func (e *settledUpgradeCleanupError) Error() string                 { return "settled cleanup fault" }
func (e *settledUpgradeCleanupError) Unwrap() error                 { return e.cause }
func (e *settledUpgradeCleanupError) RetainedStore() *taskdir.Store { return nil }

// The consumer-owned marker double tests projection only. Infrastructure tests
// separately prove that production emits it solely for a validated live owner.
type historicalWaitPendingTestError struct{ cause error }

func (e *historicalWaitPendingTestError) Error() string              { return "historical Wait fixture pending" }
func (e *historicalWaitPendingTestError) Unwrap() error              { return e.cause }
func (*historicalWaitPendingTestError) HistoricalDaemonWaitPending() {}

func canonicalUpgradeWaitError(cause error) error {
	return fmt.Errorf("advance authorized upgrade generation: %w", fmt.Errorf("%w: wait for dispatched predecessor shutdown: %w", pueue.ErrPrivateSupervisorMigrationRequired, cause))
}

// The settlement-port double supplies only test inputs. This store workflow
// test proves fencing/error projection, not native custody or Wait completion.
type upgradeWaitPendingPort struct {
	pueue.DaemonWaitOwner
	root     string
	expected lifetime.ProcessRef
	owned    bool
	cause    error
	calls    atomic.Int32
}

func (w *upgradeWaitPendingPort) ObserveDaemonWait(ctx context.Context, root string, expected lifetime.ProcessRef) (bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	same, err := lifetime.SameProcess(expected, w.expected)
	if err != nil || !same || root != w.root {
		return false, false, errors.Join(infrastructure.ErrIdentity, err)
	}
	w.calls.Add(1)
	return w.owned, false, w.cause
}

func TestUpgradeWaitPendingThroughCanonicalStoreWorkflow(t *testing.T) {
	for _, test := range []struct {
		name      string
		owned     bool
		cause     error
		condition infrastructure.UpgradeCondition
	}{
		{"private registered wait pending", true, nil, infrastructure.UpgradePending},
		{"tagged historical custodian pending", false, &historicalWaitPendingTestError{cause: infrastructure.ErrOwnerPending}, infrastructure.UpgradePending},
		{"unbranded owner marker remains fault", false, infrastructure.ErrOwnerPending, infrastructure.UpgradeFault},
	} {
		t.Run(test.name, func(t *testing.T) {
			testUpgradeWaitPendingStoreWorkflow(t, test.owned, test.cause, test.condition)
		})
	}
}

func testUpgradeWaitPendingStoreWorkflow(t *testing.T, owned bool, waitErr error, condition infrastructure.UpgradeCondition) {
	t.Helper()
	store, original, owner := newOwnedPreparationProbe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	binding := original.Binding()
	predecessor := recordedUpgradeWaitPredecessor(t, binding)
	wait := &upgradeWaitPendingPort{DaemonWaitOwner: owner, root: store.Root, expected: predecessor, owned: owned, cause: waitErr}
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, SupervisorExecutable: binding.ClientExecutable, ControlEnvironment: pueue.DefaultEnvironment(), MaintenanceEnvironment: []string{}}
	observation, starts := newUpgradeWaitPendingObservation(t, selected)
	options := pueue.Options{ObservationTimeout: 10 * time.Second, WaitForPendingClose: true, PrivateGenerationGuard: owner, DaemonWaitOwner: wait, DaemonStartGuard: observation}
	op := seedUpgradeWaitPendingJournal(t, pin, binding, predecessor)
	base := filepath.Dir(binding.ConfigPath)
	before := supervisorfixture.CapturePairFaultWitness(t, base)
	preparation := infrastructurePreparationOwner{store: store, options: options, observation: observation, lookup: installation.ExecutableLookup{InitialExecutable: binding.ClientExecutable}}
	outcome, prepareErr := preparation.performUpgrade(ctx, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: infrastructure.UpgradeOperationID(op)}, selected)
	if outcome.Condition != condition || outcome.Report.OperationID != infrastructure.UpgradeOperationID(op) || outcome.Report.Phase != infrastructure.UpgradeMigrating || outcome.Report.Completed {
		t.Fatalf("canonical Wait workflow lost pending subject: outcome=%+v err=%v", outcome, prepareErr)
	}
	if condition == infrastructure.UpgradePending && prepareErr != nil || condition == infrastructure.UpgradeFault && !errors.Is(prepareErr, waitErr) {
		t.Fatalf("canonical Wait workflow error projection: condition=%s err=%v", outcome.Condition, prepareErr)
	}
	if wait.calls.Load() == 0 || starts.Load() != 0 || observation.Disposition() != (infrastructure.Disposition{}) {
		t.Fatalf("unresolved Wait gained readiness/Start: waits=%d starts=%d disposition=%+v", wait.calls.Load(), starts.Load(), observation.Disposition())
	}
	supervisorfixture.AssertPairFaultWitness(t, base, before)
}

func recordedUpgradeWaitPredecessor(t *testing.T, binding task.SupervisorRef) lifetime.ProcessRef {
	t.Helper()
	pidText := strings.TrimSpace(string(readPreparationProbeFile(t, filepath.Join(filepath.Dir(binding.ConfigPath), "run", "pueue.pid"))))
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatal(err)
	}
	// A deliberately different fixture incarnation can be observed absent, but
	// it still cannot bypass the unresolved Wait port or gain Start permission.
	predecessor, err := lifetime.NewProcessRef(pid, "fixture-recorded-predecessor")
	if err != nil {
		t.Fatal(err)
	}
	return predecessor
}

func newUpgradeWaitPendingObservation(t *testing.T, selected infrastructure.LaunchContext) (*infrastructure.PreparationObservation, *atomic.Int32) {
	t.Helper()
	digest, err := selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	starts := new(atomic.Int32)
	guard := infrastructurePreparationGuard(func(context.Context, string, func() error) error {
		starts.Add(1)
		return pueue.ErrInfrastructureOwnerRequired
	})
	observation, err := infrastructure.NewPreparationObservation(guard, infrastructure.LaunchContextReference{SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	return observation, starts
}

// These are validated durable input fixtures for a resumed same-pair handoff.
// Production readers, transition code and the actual Host classifier own the
// behavior under test; these bytes grant no native closure or launch evidence.
func seedUpgradeWaitPendingJournal(t *testing.T, pin taskdir.RootPin, binding task.SupervisorRef, predecessor lifetime.ProcessRef) pueue.UpgradeOperationID {
	t.Helper()
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	op := pueue.UpgradeOperationID("upgrade-v1:" + strings.Repeat("7", 32))
	root := map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}
	ref := map[string]any{"operation_id": op}
	identity := map[string]any{"executable": binding.DaemonExecutable, "executable_identity": binding.ExecutableIdentity, "config_path": binding.ConfigPath, "config_digest": binding.ConfigDigest, "resolved_config_sha256": binding.ResolvedConfigSHA256, "endpoint": binding.Endpoint}
	drain := map[string]any{"root": root, "predecessor": binding, "operation_id": op, "mode": "operation_required", "phase": "migrating", "completion_authorization": ref, "migration_reference": ref, "selected_pair": map[string]any{"client_executable": binding.ClientExecutable, "daemon_executable": binding.DaemonExecutable}}
	generation := map[string]any{"operation_id": op, "root": root, "predecessor": binding, "mode": "operation_required", "target": binding, "legacy_boot_id": boot}
	migration := map[string]any{"schema_version": task.SchemaVersion, "legacy": identity, "successor": identity, "legacy_pid": predecessor.PID(), "legacy_process_start": predecessor.StartToken(), "legacy_shutdown_requested": true, "legacy_shutdown_dispatched": true, "upgrade_generation": generation}
	err = pueue.WithExistingBootstrapLockContext(t.Context(), pin.Root(), func(context.Context) error {
		for name, record := range map[string]any{"bootstrap.pending": map[string]any{"schema_version": task.SchemaVersion, "drain_v1": drain}, "migration.json": migration} {
			data, marshalErr := task.MarshalCanonical(record)
			if marshalErr != nil {
				return marshalErr
			}
			writeFixtureFile(t, filepath.Join(filepath.Dir(binding.ConfigPath), name), data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observed, found, err := pueue.ObservePrivateUpgrade(t.Context(), pin, op)
	if err != nil || !found || observed.Phase != pueue.UpgradeMigrating {
		t.Fatalf("invalid resumed upgrade fixture: found=%t observation=%+v err=%v", found, observed, err)
	}
	phase, found, err := pueue.ObservePrivateMigrationPhase(pin.Root())
	if err != nil || !found || phase != pueue.PrivateMigrationPredecessorShutdownDispatched {
		t.Fatalf("invalid dispatched migration fixture: phase=%s found=%t err=%v", phase, found, err)
	}
	return op
}

func TestUpgradeWaitPendingClassificationPreservesIndependentFaults(t *testing.T) {
	privatePending := &pueue.PrivateUpgradeError{ReasonCode: "predecessor_wait_pending", Err: pueue.ErrPrivateUpgradePending}
	privateUnknown := &pueue.PrivateUpgradeError{ReasonCode: "successor_birth_unknown", Err: errors.Join(pueue.ErrPrivateUpgradePending, pueue.ErrUnknown)}
	tagged := &historicalWaitPendingTestError{cause: infrastructure.ErrOwnerPending}
	for _, test := range []struct {
		name      string
		cause     error
		condition infrastructure.UpgradeCondition
		retained  error
	}{
		{"unbranded owner pending", infrastructure.ErrOwnerPending, infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"unbranded owner pending through migration", canonicalUpgradeWaitError(infrastructure.ErrOwnerPending), infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"private wait pending", privatePending, infrastructure.UpgradePending, nil},
		{"private wait plus required context", canonicalUpgradeWaitError(privatePending), infrastructure.UpgradeFault, pueue.ErrPrivateSupervisorMigrationRequired},
		{"private unknown birth", privateUnknown, infrastructure.UpgradePending, nil},
		{"private unknown birth plus required context", canonicalUpgradeWaitError(privateUnknown), infrastructure.UpgradeFault, pueue.ErrPrivateSupervisorMigrationRequired},
		{"independent required plus private pending", errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, privatePending), infrastructure.UpgradeFault, pueue.ErrPrivateSupervisorMigrationRequired},
		{"tagged historical custodian", tagged, infrastructure.UpgradePending, nil},
		{"wrapped tagged historical custodian", fmt.Errorf("advance authorized upgrade generation: %w", tagged), infrastructure.UpgradePending, nil},
		{"multiple tagged historians", errors.Join(tagged, &historicalWaitPendingTestError{cause: infrastructure.ErrOwnerPending}), infrastructure.UpgradePending, nil},
		{"tagged plus untagged owner", errors.Join(tagged, infrastructure.ErrOwnerPending), infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"tagged plus independent required", errors.Join(tagged, pueue.ErrPrivateSupervisorMigrationRequired), infrastructure.UpgradeFault, pueue.ErrPrivateSupervisorMigrationRequired},
		{"tagged plus unknown", errors.Join(tagged, pueue.ErrUnknown), infrastructure.UpgradeFault, pueue.ErrUnknown},
		{"tagged plus storage", errors.Join(tagged, os.ErrPermission), infrastructure.UpgradeFault, os.ErrPermission},
		{"tagged plus native uncertainty", errors.Join(tagged, lifetime.ErrUnverifiableLaunch), infrastructure.UpgradeFault, lifetime.ErrUnverifiableLaunch},
		{"tagged plus evidence fault", errors.Join(tagged, task.ErrEvidenceFault), infrastructure.UpgradeFault, task.ErrEvidenceFault},
		{"tagged plus cancellation", errors.Join(tagged, context.Canceled), infrastructure.UpgradeFault, context.Canceled},
		{"tagged plus deadline", errors.Join(tagged, context.DeadlineExceeded), infrastructure.UpgradeFault, context.DeadlineExceeded},
		{"tagged retains cleanup", &retainedPreparationFixtureError{cause: tagged}, infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"tagged settled cleanup fault", &settledUpgradeCleanupError{cause: tagged}, infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"tagged scope contains independent storage", &historicalWaitPendingTestError{cause: errors.Join(infrastructure.ErrOwnerPending, os.ErrPermission)}, infrastructure.UpgradeFault, os.ErrPermission},
		{"tagged scope contains untagged branch", &historicalWaitPendingTestError{cause: errors.Join(tagged, infrastructure.ErrOwnerPending)}, infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"bare migration required", pueue.ErrPrivateSupervisorMigrationRequired, infrastructure.UpgradeFault, pueue.ErrPrivateSupervisorMigrationRequired},
		{"owner plus unknown", errors.Join(infrastructure.ErrOwnerPending, pueue.ErrUnknown), infrastructure.UpgradeFault, pueue.ErrUnknown},
		{"owner plus native uncertainty", canonicalUpgradeWaitError(errors.Join(infrastructure.ErrOwnerPending, lifetime.ErrUnverifiableLaunch)), infrastructure.UpgradeFault, lifetime.ErrUnverifiableLaunch},
		{"owner plus evidence fault", canonicalUpgradeWaitError(errors.Join(infrastructure.ErrOwnerPending, task.ErrEvidenceFault)), infrastructure.UpgradeFault, task.ErrEvidenceFault},
		{"owner plus storage fault", canonicalUpgradeWaitError(errors.Join(infrastructure.ErrOwnerPending, os.ErrPermission)), infrastructure.UpgradeFault, os.ErrPermission},
		{"owner plus cancellation", canonicalUpgradeWaitError(errors.Join(infrastructure.ErrOwnerPending, context.Canceled)), infrastructure.UpgradeFault, context.Canceled},
		{"owner plus deadline", canonicalUpgradeWaitError(errors.Join(infrastructure.ErrOwnerPending, context.DeadlineExceeded)), infrastructure.UpgradeFault, context.DeadlineExceeded},
		{"owner retained cleanup", &retainedPreparationFixtureError{cause: infrastructure.ErrOwnerPending}, infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"owner settled cleanup fault", &settledUpgradeCleanupError{cause: infrastructure.ErrOwnerPending}, infrastructure.UpgradeFault, infrastructure.ErrOwnerPending},
		{"private pending plus cancellation", canonicalUpgradeWaitError(errors.Join(privatePending, context.Canceled)), infrastructure.UpgradeFault, context.Canceled},
		{"private pending plus native fault", canonicalUpgradeWaitError(errors.Join(privatePending, lifetime.ErrUnverifiableLaunch)), infrastructure.UpgradeFault, lifetime.ErrUnverifiableLaunch},
		{"private pending plus storage fault", canonicalUpgradeWaitError(errors.Join(privatePending, os.ErrPermission)), infrastructure.UpgradeFault, os.ErrPermission},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := upgradeOutcome(pueue.PrivateUpgradeObservation{}, test.cause)
			if outcome.Condition != test.condition || test.retained == nil && err != nil || test.retained != nil && !errors.Is(err, test.retained) {
				t.Fatalf("Wait pending classification: condition=%s want=%s err=%v", outcome.Condition, test.condition, err)
			}
		})
	}
}
