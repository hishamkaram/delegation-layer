package supervisor_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestOperatorDispositionSurvivesTwoDifferentGenerationMaintenanceAssessments(t *testing.T) {
	store, seed, template := lifecyclefixture.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	requireMaintenanceDispositionTest(t, err)
	request := *template
	request.TaskID = strings.Repeat("4", 32)
	initial := task.SupervisorRef{ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, ClientExecutable: "/installed/A/pueue", DaemonExecutable: "/installed/A/pueued", ConfigPath: pueue.PrivateConfigPath(store.Root), ConfigDigest: task.ComputeSHA256([]byte("A config")), ResolvedConfigSHA256: task.ComputeSHA256([]byte("A resolved")), Endpoint: "unix:" + filepath.Join(store.Root, "A.sock"), ObservedVersion: pueue.FixtureVersion}
	meta.TaskID = request.TaskID
	meta.SpecSHA256 = ""
	meta.SupervisorConfig = initial
	td, err := store.CreateTask(request.TaskID, &request, []byte("brief"), meta)
	requireMaintenanceDispositionTest(t, err)
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit, err := td.PrepareSubmission(initial)
	requireMaintenanceDispositionTest(t, err)
	submit, err := permit.Record()
	requireMaintenanceDispositionTest(t, err)
	requireMaintenanceDispositionTest(t, errors.Join(permit.Consume(), permit.Release()))
	id := int64(7)
	observed := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: id, Binding: initial, Identity: pueue.Identity{RootID: store.RootID, TaskID: td.TaskID, SpecSHA256: submit.SpecSHA256, MetaSHA256: submit.MetaSHA256, NumericTaskID: &id}}
	receipt, err := observed.Receipt()
	requireMaintenanceDispositionTest(t, err)
	requireMaintenanceDispositionTest(t, td.RecordSupervisorReceipt(*receipt))
	recordMaintenanceGhost(t, td)
	queue := task.OperatorDispositionQueue{NumericTaskID: id, Label: receipt.Label, Ended: true}
	_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	requireMaintenanceDispositionTest(t, err)
	_, err = td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
	requireMaintenanceDispositionTest(t, err)
	before, err := os.ReadFile(filepath.Join(td.Dir, "operator-disposition.json"))
	requireMaintenanceDispositionTest(t, err)
	cleanup := supervisor.NewCleanup(nil, nil)
	current := initial
	history := []any{}
	label := receipt.Label
	jobs := []pueue.Job{{ID: id, Label: &label, State: pueue.StateEnded, Group: "default"}}
	for _, name := range []string{"B", "C"} {
		next := initial
		next.ClientExecutable = "/installed/" + name + "/pueue"
		next.DaemonExecutable = "/installed/" + name + "/pueued"
		next.ConfigDigest = task.ComputeSHA256([]byte(name + " config"))
		next.ResolvedConfigSHA256 = task.ComputeSHA256([]byte(name + " resolved"))
		next.Endpoint = "unix:" + filepath.Join(store.Root, name+".sock")
		history = append(history, map[string]any{"schema_version": task.SchemaVersion, "legacy": maintenanceDaemonIdentity(current), "successor": maintenanceDaemonIdentity(next), "legacy_pid": 101 + len(history), "legacy_stopped": true})
		recordRuntimeUpgradeFence(t, store, next)
		writeMaintenanceRecord(t, filepath.Join(store.Root, ".supervisor", "supervisor.identity.json"), map[string]any{"schema_version": task.SchemaVersion, "binding": next})
		writeMaintenanceRecord(t, filepath.Join(store.Root, ".supervisor", "daemon.identity.json"), maintenanceDaemonIdentity(next))
		writeMaintenanceRecord(t, filepath.Join(store.Root, ".supervisor", "migration-history.json"), map[string]any{"schema_version": task.SchemaVersion, "migrations": history})
		err = pueue.WithExistingBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
			assessment, checkErr := cleanup.AssessMaintenance(ctx, store, next, jobs)
			if checkErr != nil || assessment.Readiness != "clear" {
				t.Fatalf("unchanged permission blocked next %s handoff: %+v %v", name, assessment, checkErr)
			}
			return nil
		})
		requireMaintenanceDispositionTest(t, err)
		after, readErr := os.ReadFile(filepath.Join(td.Dir, "operator-disposition.json"))
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatal("handoff rewrote immutable human permission")
		}
		proof, readErr := store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
		if readErr != nil || proof.Proof != nil {
			t.Fatal("maintenance granted generic native closure")
		}
		plan, cleanupErr := cleanup.AuthorizeSuccessfulCleanup(t.Context(), store.Root, next, jobs)
		if cleanupErr != nil || len(plan.RemoveIDs) != 0 {
			t.Fatalf("settlement granted row deletion: %+v %v", plan, cleanupErr)
		}
		current = next
	}
	if _, err = guardian.ReadClosureProof(t.Context(), td); !errors.Is(err, guardian.ErrProofPending) {
		t.Fatalf("settlement fabricated lifetime proof: %v", err)
	}
	// Independent active work still blocks the entire root.
	jobs = append(jobs, pueue.Job{ID: 8, State: pueue.StateRunning})
	if err = supervisor.CheckMaintenanceQuiescence(t.Context(), store, current, jobs); !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
		t.Fatalf("per-task permission overrode other work: %v", err)
	}
}

func recordMaintenanceGhost(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	boot, err := lifetime.BootID()
	requireMaintenanceDispositionTest(t, err)
	self, err := lifetime.ObserveProcess(os.Getpid())
	requireMaintenanceDispositionTest(t, err)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	requireMaintenanceDispositionTest(t, err)
	defer func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	deadline := time.Now().Add(time.Hour)
	_, err = td.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "maintenance-ghost", BootID: boot, Deadline: deadline})
	requireMaintenanceDispositionTest(t, err)
	_, err = td.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopeID: "maintenance-ghost", BootID: boot, Deadline: deadline})
	requireMaintenanceDispositionTest(t, err)
	_, err = td.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{PID: 1 << 30, ProcessStart: self.StartToken(), BootID: boot})
	requireMaintenanceDispositionTest(t, err)
}

func maintenanceDaemonIdentity(binding task.SupervisorRef) map[string]any {
	return map[string]any{"executable": binding.DaemonExecutable, "executable_identity": binding.ExecutableIdentity, "config_path": binding.ConfigPath, "config_digest": binding.ConfigDigest, "resolved_config_sha256": binding.ResolvedConfigSHA256, "endpoint": binding.Endpoint}
}

func writeMaintenanceRecord(t *testing.T, path string, record any) {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	requireMaintenanceDispositionTest(t, err)
	requireMaintenanceDispositionTest(t, os.WriteFile(path, data, 0o600))
}

func requireMaintenanceDispositionTest(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
