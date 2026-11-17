package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestHistoricalMaintenanceUsesSavedExitWithoutOwnerOrQueue(t *testing.T) {
	for _, name := range []string{"legacy-complete-1", "legacy-complete-2", "legacy-complete-3", "legacy-complete-4", "legacy-complete-5"} {
		t.Run(name, func(t *testing.T) {
			store, id := loadMaintenanceFixture(t, filepath.Join("testdata", "upgrade-maintenance", name))
			before, err := os.ReadDir(filepath.Join(store.Root, "inspections", id))
			mustInspection(t, err)
			err = ObserveMaintenanceContext(t.Context(), store, id, contract.MaintenanceQueue{}, nil, func(observation MaintenanceObservation) error {
				if !observation.HistoricalExit || observation.Permission || observation.Lifetime.OwnerPresent {
					t.Fatalf("unexpected maintenance verdict: historical=%v permission=%v owner=%v", observation.HistoricalExit, observation.Permission, observation.Lifetime.OwnerPresent)
				}
				return nil
			})
			mustInspection(t, err)
			after, err := os.ReadDir(filepath.Join(store.Root, "inspections", id))
			mustInspection(t, err)
			if len(before) != len(after) {
				t.Fatal("historical observation created evidence")
			}
		})
	}
}

func loadMaintenanceFixture(t *testing.T, source string) (*taskdir.Store, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(source, requestRecordName))
	mustInspection(t, err)
	var request contract.RequestRecord
	mustInspection(t, json.Unmarshal(raw, &request))
	root := filepath.Join(t.TempDir(), "state")
	mustInspection(t, os.MkdirAll(root, 0o700))
	data, err := task.MarshalCanonical(task.RootRecord{SchemaVersion: task.SchemaVersion, RootID: request.RootID, CreatedAt: "2026-01-01T00:00:00Z"})
	mustInspection(t, err)
	mustInspection(t, os.WriteFile(filepath.Join(root, "root.json"), data, 0o600))
	mustInspection(t, os.WriteFile(filepath.Join(root, ".maintenance.lock"), nil, 0o600))
	store, err := taskdir.InitStore(root)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, store.Close()) })
	dest := filepath.Join(root, "inspections", request.IdentityID())
	mustInspection(t, os.MkdirAll(dest, 0o700))
	names, err := os.ReadDir(source)
	mustInspection(t, err)
	for _, name := range names {
		if strings.HasPrefix(name.Name(), "._") {
			continue
		}
		if !name.Type().IsRegular() {
			t.Fatalf("unexpected fixture member %s", name.Name())
		}
		data, readErr := os.ReadFile(filepath.Join(source, name.Name()))
		mustInspection(t, readErr)
		mustInspection(t, os.WriteFile(filepath.Join(dest, name.Name()), data, 0o600))
	}
	op, err := LoadOperationContext(t.Context(), store, request.IdentityID())
	mustInspection(t, err)
	mustInspection(t, op.Close())
	return store, request.IdentityID()
}

func TestPrivateHistoricalMaintenanceDecode(t *testing.T) {
	source := os.Getenv("DELEGATE_PRIVATE_INSPECTION_FIXTURES")
	if source == "" {
		t.Skip("private fixture directory not supplied")
	}
	entries, err := os.ReadDir(source)
	mustInspection(t, err)
	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "._") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			store, id := loadMaintenanceFixture(t, filepath.Join(source, entry.Name()))
			mustInspection(t, ObserveMaintenanceContext(t.Context(), store, id, contract.MaintenanceQueue{}, nil, func(MaintenanceObservation) error { return nil }))
		})
		checked++
	}
	if checked != 7 {
		t.Fatalf("validated %d journals, expected seven private fixtures", checked)
	}
}

func TestHistoricalMaintenanceRejectsCorruptExitEvidence(t *testing.T) {
	for _, name := range []string{workerObservationRecordName, completionRecordName, resultRecordName, receiptRecordName} {
		t.Run(name, func(t *testing.T) {
			store, id := loadMaintenanceFixture(t, filepath.Join("testdata", "upgrade-maintenance", "legacy-complete-1"))
			mustInspection(t, os.WriteFile(filepath.Join(store.Root, "inspections", id, name), []byte("{"), 0o600))
			err := ObserveMaintenanceContext(t.Context(), store, id, contract.MaintenanceQueue{}, nil, func(MaintenanceObservation) error {
				t.Fatal("corrupt evidence reached maintenance consumer")
				return nil
			})
			if err == nil || task.RecordAbsent(err) {
				t.Fatalf("corrupt evidence returned %v", err)
			}
		})
	}
}

func TestHistoricalFailedWorkerValidatesPartialResult(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid-partial", true: "malformed-partial"}[invalid], func(t *testing.T) {
			op := evidenceOperation(t)
			_, err := op.ClaimStart(time.Unix(101, 0))
			mustInspection(t, err)
			mustInspection(t, op.RecordReceipt(42))
			result, err := newResultRecord(contract.ResultEligible, []byte(`{"eligible":true}`), op.digest, op.factsLimit())
			mustInspection(t, err)
			_, err = op.control.Put(resultRecordName, result)
			mustInspection(t, err)
			mustInspection(t, op.RecordWorkerExitContext(t.Context(), 42, false))
			if invalid {
				mustInspection(t, os.WriteFile(filepath.Join(op.store.Root, "inspections", op.request.IdentityID(), resultRecordName), []byte("{"), 0o600))
			}
			err = ObserveMaintenanceContext(t.Context(), op.store, op.request.IdentityID(), contract.MaintenanceQueue{}, nil, func(observation MaintenanceObservation) error {
				if !observation.HistoricalExit {
					t.Fatal("valid failed partial result did not establish historical exit")
				}
				return nil
			})
			if invalid {
				if err == nil {
					t.Fatal("malformed result was ignored")
				}
			} else {
				mustInspection(t, err)
				if _, err = op.ReadProof(); err == nil {
					t.Fatal("failed worker gained admission proof")
				}
			}
		})
	}
}

type maintenanceNative struct {
	live bool
	boot string
}

func (n maintenanceNative) CurrentBootID() (string, error)          { return n.boot, nil }
func (maintenanceNative) SameBoot(a, b string) bool                 { return a == b }
func (n maintenanceNative) ProcessAbsent(int, string) (bool, error) { return !n.live, nil }

func nativeMaintenanceFixture(t *testing.T) (*taskdir.Store, *Operation, taskdir.RootPin, contract.MaintenanceQueue) {
	t.Helper()
	store, request, binding := inspectionFixture(t)
	binding.WorkerExitProof = contract.WorkerExitProofGuardianWaitV1
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	op, err := OpenOperation(store, request, binding, time.Now())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	owner, err := op.AcquireLifetimeOwner(t.Context())
	mustInspection(t, err)
	facts := inspectionLifetimeRegistrationFacts(op)
	recordInspectionLifetimeForTest(t, op, facts)
	_, err = op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1234, ProcessStart: "recorded-worker", BootID: facts.BootID})
	mustInspection(t, err)
	mustInspection(t, owner.Release())
	pin, err := store.PinRootContext(t.Context())
	mustInspection(t, err)
	return store, op, pin, contract.MaintenanceQueue{Label: op.request.Label, State: contract.MaintenanceQueueAbsent}
}

func TestInspectionSettlementMaintenanceOnlyAndIdempotent(t *testing.T) {
	store, op, pin, queue := nativeMaintenanceFixture(t)
	native := maintenanceNative{boot: "inspection-boot"}
	before, err := os.ReadDir(filepath.Join(store.Root, "inspections", op.request.IdentityID()))
	mustInspection(t, err)
	_, challenge, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native)
	mustInspection(t, err)
	after, err := os.ReadDir(filepath.Join(store.Root, "inspections", op.request.IdentityID()))
	mustInspection(t, err)
	if len(before) != len(after) {
		t.Fatal("prepare wrote evidence")
	}
	winner, err := op.ConfirmMaintenanceDispositionContext(t.Context(), pin, queue, challenge, native)
	mustInspection(t, err)
	readWinner, readErr := op.ReadMaintenanceDispositionContext(t.Context(), pin)
	mustInspection(t, readErr)
	if *readWinner != *winner {
		t.Fatal("immutable reader changed winner")
	}
	repeated, err := op.ConfirmMaintenanceDispositionContext(t.Context(), pin, contract.MaintenanceQueue{}, challenge, nil)
	mustInspection(t, err)
	if *winner != *repeated {
		t.Fatal("repeated confirmation changed winner")
	}
	mustInspection(t, ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), queue, native, func(observation MaintenanceObservation) error {
		if !observation.Permission || observation.Disposition == nil || observation.HistoricalExit {
			t.Fatal("settlement maintenance permission missing")
		}
		evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), observation.Lifetime.Records)
		if err != nil {
			return err
		}
		closure, err := task.ResolveLifetimeClosure(evidence)
		if err != nil {
			return err
		}
		if closure.Closed() {
			t.Fatal("settlement fabricated native closure")
		}
		return nil
	}))
}

func TestInspectionSettlementRefusesLiveOrChangedBoot(t *testing.T) {
	_, op, pin, queue := nativeMaintenanceFixture(t)
	for _, native := range []maintenanceNative{{live: true, boot: "inspection-boot"}, {boot: "different-boot"}} {
		if _, _, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native); err == nil {
			t.Fatal("unsafe settlement accepted")
		}
	}
	_, challenge, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, maintenanceNative{boot: "inspection-boot"})
	mustInspection(t, err)
	if _, err = op.ConfirmMaintenanceDispositionContext(t.Context(), pin, queue, strings.Repeat("f", 64), maintenanceNative{boot: "inspection-boot"}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("wrong challenge: %v", err)
	}
	if challenge == "" {
		t.Fatal("challenge missing")
	}
}

func TestMaintenanceSettlementEligibilityUsesPreparationOwner(t *testing.T) {
	store, op, _, _ := nativeMaintenanceFixture(t)
	for _, native := range []taskdir.OperatorDispositionNative{nil, maintenanceNative{boot: "inspection-boot"}} {
		mustInspection(t, ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, native, func(observed MaintenanceObservation) error {
			if observed.SettlementEligible != (native != nil) || observed.Permission {
				t.Fatal("eligibility was not derived from native preparation evidence")
			}
			return nil
		}))
	}
}

func TestMaintenanceAutomaticEvidencePrecedesSettlementEligibility(t *testing.T) {
	for _, name := range []string{"legacy-complete-1", "current-unstarted-1"} {
		t.Run(name, func(t *testing.T) {
			store, id := loadMaintenanceFixture(t, filepath.Join("testdata", "upgrade-maintenance", name))
			mustInspection(t, ObserveMaintenanceContext(t.Context(), store, id, contract.MaintenanceQueue{}, maintenanceNative{boot: "unrelated-boot", live: true}, func(observed MaintenanceObservation) error {
				if observed.SettlementEligible || (!observed.HistoricalExit && !observed.Lifetime.PreparationOnly) {
					t.Fatal("automatic evidence acquired settlement requirements")
				}
				return nil
			}))
		})
	}
}

func TestMaintenanceAfterUnlaunchedRecoveryFencePrecedesSettlementEligibility(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.WorkerExitProof = contract.WorkerExitProofGuardianWaitV1
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	created := time.Now().UTC().Add(-2 * AdmissionTimeout)
	op, err := OpenOperationContext(t.Context(), store, request, binding, created)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	_, err = op.ClaimSubmissionContext(t.Context(), created.Add(time.Second))
	mustInspection(t, err)
	_, err = op.SealLifetimeUnlaunchedContext(t.Context())
	mustInspection(t, err)
	native := maintenanceNative{boot: "unrelated-boot", live: true}
	mustInspection(t, ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, native, func(observed MaintenanceObservation) error {
		if !observed.canonicalClosed || observed.SettlementEligible || observed.HistoricalExit || observed.Lifetime.PreparationOnly || observed.Permission {
			t.Fatal("canonical unlaunched recovery acquired settlement requirements")
		}
		return nil
	}))
}

// These records exercise passive policy only: no cgroup or process is created.
func pendingMaintenanceFixture(t *testing.T, kind string) (*taskdir.Store, *Operation, taskdir.RootPin, contract.MaintenanceQueue) {
	t.Helper()
	store, request, binding := inspectionFixture(t)
	binding.WorkerExitProof, binding.RunnerOwnership = contract.WorkerExitProofGuardianWaitV1, task.RunnerOwnershipManaged
	op, err := OpenOperationContext(t.Context(), store, request, binding, time.Now())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	owner, err := op.AcquireLifetimeOwner(t.Context())
	mustInspection(t, err)
	facts := inspectionLifetimeRegistrationFacts(op)
	facts.BootID = "linux:11111111-1111-1111-1111-111111111111"
	if kind == "exact" {
		facts.ScopeKind, facts.Assurance = task.LifetimeScopeCgroupV2, task.LifetimeAssuranceExact
	}
	_, err = op.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{ScopeKind: facts.ScopeKind, Assurance: facts.Assurance, ScopePlan: "pending-inspection-policy", BootID: facts.BootID, Deadline: facts.Deadline})
	mustInspection(t, err)
	if kind != "intent-only" {
		_, err = op.RecordLifetimeRegistrationContext(t.Context(), facts)
		mustInspection(t, err)
	}
	if kind != "intent-only" && kind != "registered-before-worker" {
		_, err = op.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{PID: 1234, ProcessStart: "recorded-worker", BootID: facts.BootID})
		mustInspection(t, err)
	}
	mustInspection(t, owner.Release())
	pin, err := store.PinRootContext(t.Context())
	mustInspection(t, err)
	return store, op, pin, contract.MaintenanceQueue{Label: op.request.Label, State: contract.MaintenanceQueueAbsent}
}

func TestMaintenanceTrackedRecoveryDoesNotRequireSettlement(t *testing.T) {
	for _, kind := range []string{"intent-only", "registered-before-worker", "exact", "previous-boot-cooperative"} {
		t.Run(kind, func(t *testing.T) {
			store, op, pin, queue := pendingMaintenanceFixture(t, kind)
			native := maintenanceNative{boot: "linux:11111111-1111-1111-1111-111111111111"}
			if kind == "previous-boot-cooperative" {
				native.boot = "linux:22222222-2222-2222-2222-222222222222"
			}
			mustInspection(t, ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, native, func(observed MaintenanceObservation) error {
				if !observed.RecoveryPending || observed.SettlementEligible || observed.Permission || observed.canonicalClosed {
					t.Fatal("valid tracked recovery became settlement or closure")
				}
				return nil
			}))
			if _, _, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native); err == nil {
				t.Fatal("tracked recovery was accepted as human settlement")
			}
		})
	}
}

func TestMaintenancePreviousBootDispositionRoutesCanonicalRecovery(t *testing.T) {
	store, op, pin, queue := pendingMaintenanceFixture(t, "previous-boot-cooperative")
	beforeReboot := maintenanceNative{boot: "linux:11111111-1111-1111-1111-111111111111"}
	_, challenge, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, beforeReboot)
	mustInspection(t, err)
	_, err = op.ConfirmMaintenanceDispositionContext(t.Context(), pin, queue, challenge, beforeReboot)
	mustInspection(t, err)
	afterReboot := maintenanceNative{boot: "linux:22222222-2222-2222-2222-222222222222"}
	mustInspection(t, ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, afterReboot, func(observed MaintenanceObservation) error {
		if !observed.RecoveryPending || observed.Disposition != nil || observed.SettlementEligible || observed.Permission || observed.canonicalClosed {
			t.Fatal("previous-boot human permission displaced canonical recovery")
		}
		return nil
	}))
	if _, _, err = op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, afterReboot); err == nil {
		t.Fatal("previous-boot disposition made preparation eligible")
	}
}

func TestMaintenancePreviousBootIgnoresPriorDevicePermissionOnlyForObservation(t *testing.T) {
	store, op, pin, queue := pendingMaintenanceFixture(t, "previous-boot-cooperative")
	beforeReboot := maintenanceNative{boot: "linux:11111111-1111-1111-1111-111111111111"}
	subject, _, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, beforeReboot)
	mustInspection(t, err)
	// Seed a contract-valid permission from a different physical device. This
	// is historical policy data, not proof of a native reboot or device change.
	subject.RootDevice ^= 1
	challenge, err := contract.MaintenanceDispositionChallenge(subject)
	mustInspection(t, err)
	prior := contract.MaintenanceDispositionRecord{SchemaVersion: task.SchemaVersion, Subject: subject, Challenge: challenge, ConfirmedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	mustInspection(t, contract.ValidateMaintenanceDisposition(&prior))
	created, err := op.control.Put(maintenanceDispositionName, prior)
	mustInspection(t, err)
	if !created {
		t.Fatal("prior-device permission fixture was not newly seeded")
	}
	if _, err = op.ReadMaintenanceDispositionContext(t.Context(), pin); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("historical reader relaxed the physical pin: %v", err)
	}
	if _, err = op.ConfirmMaintenanceDispositionContext(t.Context(), pin, queue, challenge, beforeReboot); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("confirmation relaxed the physical pin: %v", err)
	}
	if err = ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, beforeReboot, func(MaintenanceObservation) error { return nil }); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("same-boot observation relaxed the physical pin: %v", err)
	}
	afterReboot := maintenanceNative{boot: "linux:22222222-2222-2222-2222-222222222222"}
	mustInspection(t, ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, afterReboot, func(observed MaintenanceObservation) error {
		if !observed.RecoveryPending || observed.Disposition != nil || observed.SettlementEligible || observed.Permission || observed.canonicalClosed {
			t.Fatal("prior-device optional permission blocked or authorized previous-boot recovery")
		}
		return nil
	}))
	if _, _, err = op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, afterReboot); err == nil {
		t.Fatal("previous-boot prior-device permission authorized preparation")
	}
}

func TestMaintenanceTrackedRecoveryStillFaultsOnUnprovableFacts(t *testing.T) {
	for _, fault := range []string{"unknown-boot", "malformed-worker-link", "invalid-claim", "unclassified-record", "missing-owner"} {
		t.Run(fault, func(t *testing.T) {
			store, op, _, _ := pendingMaintenanceFixture(t, "exact")
			dir := filepath.Join(store.Root, "inspections", op.request.IdentityID())
			native := maintenanceNative{boot: "linux:11111111-1111-1111-1111-111111111111"}
			switch fault {
			case "unknown-boot":
				native.boot = "unknown-legacy-boot"
			case "malformed-worker-link":
				worker, err := op.ReadLifetimeWorkerContext(t.Context())
				mustInspection(t, err)
				worker.RegistrationSHA256 = task.ComputeSHA256([]byte("different-registration"))
				data, err := task.MarshalCanonical(worker)
				mustInspection(t, err)
				mustInspection(t, os.WriteFile(filepath.Join(dir, inspectionLifetimeWorkerName), data, 0o600))
			case "invalid-claim":
				mustInspection(t, os.WriteFile(filepath.Join(dir, startRecordName), []byte("{}"), 0o600))
			case "unclassified-record":
				mustInspection(t, os.WriteFile(filepath.Join(dir, "unknown.json"), []byte("{}"), 0o600))
			case "missing-owner":
				mustInspection(t, os.Remove(filepath.Join(dir, ".lifetime-owner.lock")))
			}
			called := false
			err := ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, native, func(MaintenanceObservation) error { called = true; return nil })
			var fact *task.ControlFactError
			if err == nil || called || !errors.As(err, &fact) || fact.Name == "" || fact.ResourceID != op.request.IdentityID() || errors.Is(err, task.ErrLockBusy) || errors.Is(err, guardian.ErrProofPending) {
				t.Fatalf("tracked recovery hid a malformed or unobservable fact: callback=%v error=%v", called, err)
			}
		})
	}
}

func TestMaintenanceSettlementEligibilityRejectsPreparationRefusals(t *testing.T) {
	for _, name := range []string{inspectionLifetimeIntentName, startRecordName, completionRecordName, "unknown.json", ".lifetime-owner.lock", "different-boot", "live-worker"} {
		t.Run(name, func(t *testing.T) {
			store, op, pin, queue := nativeMaintenanceFixture(t)
			native := maintenanceNative{boot: "inspection-boot"}
			dir := filepath.Join(store.Root, "inspections", op.request.IdentityID())
			switch name {
			case inspectionLifetimeIntentName, ".lifetime-owner.lock":
				mustInspection(t, os.Remove(filepath.Join(dir, name)))
			case "different-boot":
				native.boot = "another-boot"
			case "live-worker":
				native.live = true
			default:
				mustInspection(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600))
			}
			if _, _, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native); err == nil {
				t.Fatal("invalid fixture unexpectedly allowed preparation")
			}
			called := false
			err := ObserveMaintenanceContext(t.Context(), store, op.request.IdentityID(), contract.MaintenanceQueue{}, native, func(MaintenanceObservation) error {
				called = true
				return nil
			})
			if err == nil || called {
				t.Fatalf("preparation refusal exposed eligibility: callback=%v error=%v", called, err)
			}
			if name != "live-worker" {
				var fact *task.ControlFactError
				if !errors.As(err, &fact) || fact.ResourceKind != "inspection" || fact.ResourceID != op.request.IdentityID() || fact.Name == "" {
					t.Fatalf("ineligible evidence lost its typed fact: %v", err)
				}
			}
		})
	}
}

var _ taskdir.OperatorDispositionNative = maintenanceNative{}

func TestInspectionSettlementRejectsStaleInventoryAndBusyOwner(t *testing.T) {
	_, op, pin, queue := nativeMaintenanceFixture(t)
	native := maintenanceNative{boot: "inspection-boot"}
	owner, err := op.TryAcquireLifetimeOwner(t.Context())
	mustInspection(t, err)
	if _, _, err = op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("owned inspection returned %v", err)
	}
	mustInspection(t, owner.Release())
	_, challenge, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native)
	mustInspection(t, err)
	result, err := newResultRecord(contract.ResultUnavailable, nil, op.digest, op.factsLimit())
	mustInspection(t, err)
	_, err = op.control.Put(resultRecordName, result)
	mustInspection(t, err)
	if _, err = op.ConfirmMaintenanceDispositionContext(t.Context(), pin, queue, challenge, native); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed inventory confirmation returned %v", err)
	}
}

func TestInspectionSettlementMissingOwnerCannotCreateEvidence(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Now())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	pin, err := store.PinRootContext(t.Context())
	mustInspection(t, err)
	queue := contract.MaintenanceQueue{Label: op.request.Label, State: contract.MaintenanceQueueAbsent}
	if _, _, err = op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, maintenanceNative{boot: "inspection-boot"}); err == nil {
		t.Fatal("missing historical ownership accepted")
	}
	if _, err = os.Stat(filepath.Join(store.Root, "inspections", request.TaskID, ".lifetime-owner.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepare provisioned lifetime ownership: %v", err)
	}
}

var errInspectionDispositionBarrier = errors.New("injected inspection disposition barrier failure")

type maintenanceDispositionBarrierFault struct {
	taskdir.BaseFaultInjector
	failed bool
}

func (f *maintenanceDispositionBarrierFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == maintenanceDispositionName && !f.failed {
		f.failed = true
		return errInspectionDispositionBarrier
	}
	return nil
}

func TestInspectionSettlementPostLinkFaultRetainsExactWinner(t *testing.T) {
	store, op, pin, queue := nativeMaintenanceFixture(t)
	native := maintenanceNative{boot: "inspection-boot"}
	_, challenge, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native)
	mustInspection(t, err)
	fault := &maintenanceDispositionBarrierFault{}
	store.SetFaultInjector(fault)
	winner, confirmErr := op.ConfirmMaintenanceDispositionContext(t.Context(), pin, queue, challenge, native)
	store.SetFaultInjector(nil)
	if !fault.failed || !errors.Is(confirmErr, errInspectionDispositionBarrier) || winner == nil {
		t.Fatalf("post-link result: fault=%v winner=%v err=%v", fault.failed, winner != nil, confirmErr)
	}
	repeated, err := op.ConfirmMaintenanceDispositionContext(t.Context(), pin, contract.MaintenanceQueue{}, challenge, nil)
	mustInspection(t, err)
	if *repeated != *winner {
		t.Fatal("ambiguous publication changed immutable winner")
	}
}

func TestInspectionSettlementDoesNotIgnoreUnrelatedStage(t *testing.T) {
	store, op, pin, queue := nativeMaintenanceFixture(t)
	native := maintenanceNative{boot: "inspection-boot"}
	_, challenge, err := op.PrepareMaintenanceDispositionContext(t.Context(), pin, queue, native)
	mustInspection(t, err)
	_, err = op.ConfirmMaintenanceDispositionContext(t.Context(), pin, queue, challenge, native)
	mustInspection(t, err)
	stage := filepath.Join(store.Root, "inspections", op.request.IdentityID(), "stage."+strings.Repeat("a", 32)+".tmp")
	mustInspection(t, os.WriteFile(stage, []byte(`{"unrelated":true}`), 0o600))
	if _, err = op.ReadMaintenanceDispositionContext(t.Context(), pin); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unrelated staging evidence was ignored: %v", err)
	}
}
