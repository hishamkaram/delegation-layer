package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func coldUpgradeFixture(t *testing.T) (generationGuardFixture, *coldUpgradeWaitOwner, PrivateUpgradeObservation) {
	t.Helper()
	f, _, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	f.client.options.ControlOwnerExecutable = ""
	f.client.options.DaemonStartGuard = allowDaemonStartForTest{}
	owner := &coldUpgradeWaitOwner{}
	owner.legacy = daemonWaitOwnerFunc(func(string, int, <-chan struct{}) error { return nil })
	f.client.options.DaemonWaitOwner = owner
	f.client.options.PrivateMigrationAuthorizer = func(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) error {
		for _, job := range jobs {
			if job.State != StateEnded {
				return ErrPrivateSupervisorMigrationBusy
			}
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(f.base, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.base, "data", "state.json"), []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, started := range owner.starts {
			if !joinDaemonWait(t, started.done) {
				t.Error("cold fixture actual Wait did not finish")
			}
		}
	})
	return f, owner, accepted
}

func TestUpgradeColdPredecessorUnknownBirthDoesNotAdoptReadyEndpoint(t *testing.T) {
	f, _, accepted := coldUpgradeFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.base, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.client.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
		t.Fatal("unknown accepted birth launched again")
		return ErrBinding
	})
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		before, found, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil || !found {
			return errors.Join(ErrBinding, readErr)
		}
		unknown := before
		unknown.PredecessorStart = privateUpgradePredecessorStartRecord{Version: 1, BootID: boot}
		if writeErr := replacePrivateUpgrade(ctx, pin, before, unknown, f.client.options); writeErr != nil {
			return writeErr
		}
		bytesBefore := readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))
		observed, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, accepted.OperationID, f.client.options)
		var refusal *PrivateUpgradeError
		if !errors.As(advanceErr, &refusal) || refusal.ReasonCode != "predecessor_birth_unknown" || observed.Phase != UpgradeDraining {
			t.Fatalf("ready endpoint adopted unknown Start: %+v err=%v", observed, advanceErr)
		}
		if !bytes.Equal(bytesBefore, readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))) {
			t.Fatal("unknown Start observation changed retained drain")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeColdPredecessorReceiptCannotBeRewoundOrRebound(t *testing.T) {
	f, _, accepted := coldUpgradeFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		before, _, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil {
			return readErr
		}
		unknown := before
		unknown.PredecessorStart = privateUpgradePredecessorStartRecord{Version: 1, BootID: boot}
		if writeErr := replacePrivateUpgrade(ctx, pin, before, unknown, f.client.options); writeErr != nil {
			return writeErr
		}
		if writeErr := replacePrivateUpgrade(ctx, pin, unknown, before, f.client.options); !errors.Is(writeErr, ErrBinding) {
			t.Fatalf("ordinary progress erased acceptance: %v", writeErr)
		}
		born := unknown
		born.PredecessorStart.PID, born.PredecessorStart.ProcessStart = 123, "immutable-recorded-birth"
		if writeErr := replacePrivateUpgrade(ctx, pin, unknown, born, f.client.options); writeErr != nil {
			return writeErr
		}
		changed := born
		changed.PredecessorStart.PID++
		if writeErr := replacePrivateUpgrade(ctx, pin, born, changed, f.client.options); !errors.Is(writeErr, ErrBinding) {
			t.Fatalf("ordinary progress rebound birth: %v", writeErr)
		}
		advanced := born
		advanced.Phase = UpgradeAwaitingInstallation
		if writeErr := replacePrivateUpgrade(ctx, pin, born, advanced, f.client.options); writeErr != nil {
			return writeErr
		}
		changed = advanced
		changed.PredecessorStart = privateUpgradePredecessorStartRecord{}
		if writeErr := replacePrivateUpgrade(ctx, pin, advanced, changed, f.client.options); !errors.Is(writeErr, ErrBinding) {
			t.Fatalf("advanced drain erased receipt: %v", writeErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeColdPredecessorPreflightRefusalsNeverAcceptStart(t *testing.T) {
	for _, kind := range []string{"missing-pair", "runnable-restore", "corrupt-restore", "rowless-native-claim", "custody"} {
		t.Run(kind, func(t *testing.T) {
			f, owner, accepted := coldUpgradeFixture(t)
			configureColdUpgradeRefusal(t, f, kind)
			pin, err := readControlRootPin(t.Context(), f.root)
			if err != nil {
				t.Fatal(err)
			}
			before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))
			err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
				_, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, accepted.OperationID, f.client.options)
				return advanceErr
			})
			if err == nil || len(owner.starts) != 0 {
				t.Fatalf("blocked preflight accepted Start: starts=%d err=%v", len(owner.starts), err)
			}
			if !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))) {
				t.Fatal("blocked preflight consumed acceptance")
			}
		})
	}
}

func configureColdUpgradeRefusal(t *testing.T, f generationGuardFixture, kind string) {
	t.Helper()
	state := filepath.Join(f.base, "data", "state.json")
	switch kind {
	case "missing-pair":
		if err := os.Remove(f.client.Binding().DaemonExecutable); err != nil {
			t.Fatal(err)
		}
	case "runnable-restore":
		data, err := os.ReadFile("../../testdata/status-4.0.4-queued.json")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(state, data, 0o600); err != nil {
			t.Fatal(err)
		}
	case "corrupt-restore":
		if err := os.WriteFile(state, []byte("invalid native state"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "rowless-native-claim":
		f.client.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
			return ErrPrivateSupervisorMigrationBusy
		}
	case "custody":
		f.client.options.DaemonWaitOwner = &coldUpgradeRefusedCustodyOwner{}
	}
}

type coldUpgradeRefusedCustodyOwner struct{ coldUpgradeWaitOwner }

func (*coldUpgradeRefusedCustodyOwner) RequireRootDaemonWaitQuiescence(context.Context, string) error {
	return ErrUnknown
}

func TestUpgradeColdPredecessorFinalValidationRefusalRollsBackUnstarted(t *testing.T) {
	f, owner, accepted := coldUpgradeFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	guardCalls := 0
	f.client.options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		guardCalls++
		path := filepath.Join(f.base, "data", "state.json")
		if writeErr := os.WriteFile(path, []byte("{\"tasks\":{},\"groups\":{}}\n"), 0o600); writeErr != nil {
			return writeErr
		}
		return start()
	})
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, accepted.OperationID, f.client.options)
		return advanceErr
	})
	var refusal *PrivateUpgradeError
	if !errors.As(err, &refusal) || refusal.ReasonCode != "predecessor_restore_changed" || guardCalls != 1 || len(owner.starts) != 0 {
		t.Fatalf("source replacement did not refuse inside actual guard before Start: calls=%d starts=%d err=%v", guardCalls, len(owner.starts), err)
	}
	if !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))) {
		t.Fatal("definite unstarted validation refusal exhausted the drain")
	}
}

func TestUpgradeColdPredecessorAcceptanceFaultCannotLaunch(t *testing.T) {
	f, owner, accepted := coldUpgradeFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("acceptance durability result lost")
	f.client.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		before, found, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil || !found {
			return errors.Join(ErrBinding, readErr)
		}
		writeErr := commit()
		winner, present, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil || !present {
			return errors.Join(writeErr, ErrBinding, readErr)
		}
		if !before.PredecessorStart.accepted() && winner.PredecessorStart.accepted() {
			return errors.Join(writeErr, fault)
		}
		return writeErr
	})
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, accepted.OperationID, f.client.options)
		return advanceErr
	})
	if !errors.Is(err, fault) || len(owner.starts) != 0 {
		t.Fatalf("ambiguous acceptance write launched a child or erased fault: starts=%d err=%v", len(owner.starts), err)
	}
	if !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))) {
		t.Fatal("still-live proven-unstarted acceptance did not restore the exact prior drain")
	}
}

func TestUpgradeColdPredecessorCancellationAfterStartRetainsBirth(t *testing.T) {
	f, owner, accepted := coldUpgradeFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.client.options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		startErr := start()
		cancel()
		return startErr
	})
	err = WithExistingBootstrapLockContext(ctx, f.root, func(locked context.Context) error {
		_, advanceErr := AdvancePrivateUpgradeDrainWithLock(locked, pin, accepted.OperationID, f.client.options)
		return advanceErr
	})
	if err == nil || len(owner.starts) != 1 {
		t.Fatalf("canceled Start lost actual child ownership: starts=%d err=%v", len(owner.starts), err)
	}
	record, found, readErr := readPrivateUpgrade(pin, accepted.OperationID)
	if readErr != nil || !found || record.PredecessorStart.PID <= 0 || record.PredecessorStart.ProcessStart == "" {
		t.Fatalf("cancellation erased durable before-reap birth: %+v found=%t err=%v", record.PredecessorStart, found, readErr)
	}
}

func TestUpgradeRecoveredMigrationCannotCaptureAnotherProcess(t *testing.T) {
	const boot = "linux:01234567-89ab-cdef-0123-456789abcdef"
	birth := privateUpgradePredecessorStartRecord{Version: 1, BootID: boot, PID: 123, ProcessStart: "recorded-born-predecessor"}
	migration := privateMigrationRecord{LegacyPID: birth.PID, LegacyProcessStart: birth.ProcessStart}
	migration.UpgradeGeneration.LegacyBootID = boot
	if err := validatePrivateUpgradeRecoveredPredecessor(birth, migration); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*privateMigrationRecord){
		func(record *privateMigrationRecord) { record.LegacyPID++ },
		func(record *privateMigrationRecord) { record.LegacyProcessStart += "-reused" },
		func(record *privateMigrationRecord) {
			record.UpgradeGeneration.LegacyBootID = "linux:ffffffff-89ab-cdef-0123-456789abcdef"
		},
	} {
		other := migration
		mutate(&other)
		if err := validatePrivateUpgradeRecoveredPredecessor(birth, other); !errors.Is(err, ErrBinding) {
			t.Fatalf("migration captured another recovered incarnation: %v", err)
		}
	}
}

func TestUpgradeColdPredecessorBornFailureNeverReacceptsStart(t *testing.T) {
	for _, kind := range []string{"boot-changed", "boot-unknown", "process-dead", "wait-closed"} {
		t.Run(kind, func(t *testing.T) { testColdUpgradeBornRefusal(t, kind) })
	}
}

func testColdUpgradeBornRefusal(t *testing.T, kind string) {
	t.Helper()
	f, owner, accepted := coldUpgradeFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	birth, reason := exhaustedColdUpgradeReceipt(t, kind)
	if kind == "wait-closed" {
		f.client.options.DaemonWaitOwner = &coldUpgradeClosedWaitOwner{}
	}
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		drain, _, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil {
			return readErr
		}
		// Durable prior-start fixture: its byte-valid receipt represents
		// already completed acceptance and birth from an earlier invocation.
		drain.PredecessorStart = birth
		if writeErr := writePrivateUpgrade(f.root, drain, true); writeErr != nil {
			return writeErr
		}
		before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))
		_, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, accepted.OperationID, f.client.options)
		var refusal *PrivateUpgradeError
		if !errors.As(advanceErr, &refusal) || refusal.ReasonCode != reason || len(owner.starts) != 0 {
			t.Fatalf("exhausted receipt was replayed or misclassified: starts=%d err=%v", len(owner.starts), advanceErr)
		}
		if !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))) {
			t.Fatal("exhausted receipt changed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func exhaustedColdUpgradeReceipt(t *testing.T, kind string) (privateUpgradePredecessorStartRecord, string) {
	t.Helper()
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	receipt := privateUpgradePredecessorStartRecord{Version: 1, BootID: boot, PID: self.PID(), ProcessStart: self.StartToken()}
	switch kind {
	case "boot-changed":
		receipt.BootID = boot[:len(boot)-1] + "0"
		if lifetime.CompareBootIDs(boot, receipt.BootID) == lifetime.BootSame {
			receipt.BootID = boot[:len(boot)-1] + "1"
		}
		return receipt, "predecessor_boot_changed"
	case "boot-unknown":
		receipt.BootID = "darwin:boot-session:01234567-89ab-cdef-0123-456789abcdef"
		if lifetime.CompareBootIDs(boot, receipt.BootID) != lifetime.BootUnknown {
			receipt.BootID = "linux:01234567-89ab-cdef-0123-456789abcdef"
		}
		return receipt, "predecessor_boot_unknown"
	case "process-dead":
		closed := closedColdUpgradeProcess(t)
		receipt.PID, receipt.ProcessStart = closed.PID(), closed.StartToken()
		return receipt, "predecessor_not_ready"
	case "wait-closed":
		return receipt, "predecessor_wait_closed"
	default:
		t.Fatalf("unknown receipt fixture %q", kind)
		return receipt, ""
	}
}

// Capture an actual native birth before its sole Wait, then retain that exact
// closed incarnation. Mutating native token text can produce an invalid token
// rather than a different or closed process, especially on macOS.
func closedColdUpgradeProcess(t *testing.T) lifetime.ProcessRef {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "closed-predecessor")
	writeExecutable(t, executable, "#!/bin/sh\nexit 0\n")
	command := exec.Command(executable)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	birth, birthErr := lifetime.ObserveStartedProcess(command.Process.Pid)
	waitErr := command.Wait() // Always join the successful Start, including observation failure.
	if err := errors.Join(birthErr, waitErr); err != nil {
		t.Fatal(err)
	}
	if err := lifetime.ValidateProcessIdentityForHost(birth); err != nil {
		t.Fatal(err)
	}
	return birth
}

type coldUpgradeClosedWaitOwner struct{ coldUpgradeWaitOwner }

func (*coldUpgradeClosedWaitOwner) ObserveDaemonWait(context.Context, string, lifetime.ProcessRef) (bool, bool, error) {
	return true, true, nil
}
