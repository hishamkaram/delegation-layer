package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// rebootLifetimeStage names the durable native-lifetime evidence a predecessor
// carries when its machine rebooted during the provider turn.
type rebootLifetimeStage int

const (
	// rebootLifetimeNone records no lifetime journal at all.
	rebootLifetimeNone rebootLifetimeStage = iota
	// rebootLifetimeOpen records intent, registration, and worker only.
	rebootLifetimeOpen
	// rebootLifetimeClosedPreviousBoot adds the previous-boot empty-scope
	// termination and the previous-boot worker absence disposition.
	rebootLifetimeClosedPreviousBoot
	// rebootLifetimeClosedBudget closes the registered scope natively: the
	// guardian recorded a budget empty-scope termination, then the worker's
	// native Wait exited with an error.
	rebootLifetimeClosedBudget
	// rebootLifetimeClosedRequested is the same native closure after a user
	// stop won the recorded termination cause.
	rebootLifetimeClosedRequested
)

const (
	rebootPreviousBootID = "reboot-previous-boot"
	rebootCurrentBootID  = "reboot-current-boot"
	rebootWorkerPID      = 4242
)

// rebootSetup selects which production records the predecessor receives. The
// provider never published outcome.json or a stop request, and published
// provider.exit only when seal is set.
type rebootSetup struct {
	// prior makes the predecessor itself a continuation holding the session
	// claim. Nil is a first-turn predecessor that owns no claim.
	prior *task.PriorSession
	// identity records the provider identity (provider.ref.json).
	identity bool
	stage    rebootLifetimeStage
	// holdRunnerLease keeps the first-turn runner lease acquired.
	holdRunnerLease bool
	// launchEnvironment records the launch environment a successor reuses.
	launchEnvironment bool
	// seal writes raw output and provider.exit under the consumed start
	// permit, so the reboot left sealed evidence that was never collected.
	seal bool
}

// rebootFixture is the historical private-supervisor fixture whose only queue
// row is Ended, plus an optional unclaimed successor over the same session.
type rebootFixture struct {
	*historicalSupervisorFixture
	successor *taskdir.TaskDir
}

func newRebootFixture(t *testing.T, setup rebootSetup) *rebootFixture {
	t.Helper()
	base := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, base)
	base.options = newHistoricalSupervisorOptions()
	prepareRebootPredecessorTask(t, base, workspace, setup)
	writeRebootEndedStatus(t, base)
	prepareCompleteStashedObservationClient(t, base)
	recordRebootRunnerEvidence(t, base, setup)
	fixture := &rebootFixture{historicalSupervisorFixture: base}
	if setup.prior != nil {
		prior := &task.PriorSession{Provider: base.req.Provider, ConversationID: setup.prior.ConversationID, PredecessorTaskID: base.req.TaskID}
		fixture.successor, _ = newAppTaskInStore(t, base.store, strings.Repeat("d", 32), false, prior)
		t.Cleanup(func() {
			if err := fixture.successor.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	return fixture
}

func prepareRebootPredecessorTask(t *testing.T, fixture *historicalSupervisorFixture, workspace string, setup rebootSetup) {
	t.Helper()
	bindHistoricalSupervisorFixture(t, fixture)
	store := fixture.store
	brief := []byte("reboot continuation predecessor")
	request := newHistoricalSupervisorTaskRecord(store.RootID, workspace, brief)
	request.PriorSession = setup.prior
	profile := appTestExecutionProfile(&request, fixture.provider, nil)
	meta := tasklifecycle.NewMeta(request, profile, fixture.predecessor, fixture.runner, task.RunnerOwnershipCustom, "reboot-continuation-test")
	if setup.launchEnvironment {
		meta.Environment, meta.EnvironmentRecorded = []string{"LANG=C"}, true
	}
	td, err := store.CreateTask(request.TaskID, &request, brief, &meta)
	fixture.td = td
	fixture.component.ownTask(td)
	if err != nil {
		t.Fatal(err)
	}
	persistedRequest, persistedMeta, err := fixture.td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	fixture.req, fixture.meta = *persistedRequest, *persistedMeta
	recordHistoricalSupervisorAdmission(t, fixture)
}

// writeRebootEndedStatus publishes the Done/Killed row the queue reports for a
// job that was running when the machine rebooted.
func writeRebootEndedStatus(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	const statusTime = "2026-09-14T00:00:00.123456789Z"
	label := fmt.Sprintf("delegate:%s:%s", fixture.req.RootID, fixture.req.TaskID)
	job := map[string]any{
		"id": 7, "created_at": statusTime, "original_command": "runner", "command": "runner", "path": "/tmp", "envs": map[string]string{}, "group": "default", "dependencies": []int64{}, "priority": int32(0), "label": label,
		"status": map[string]any{"Done": map[string]any{"enqueued_at": statusTime, "start": statusTime, "end": statusTime, "result": "Killed"}},
	}
	data, err := json.Marshal(map[string]any{"tasks": map[string]any{"7": job}, "groups": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fixture.statusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := pueue.ParseStatus(readSupervisorProbeTestFile(t, fixture.statusPath), fixture.predecessor.ObservedVersion)
	if err != nil || len(jobs) != 1 || jobs[0].ID != 7 || jobs[0].State != pueue.StateEnded {
		t.Fatalf("fixture did not publish an actual Ended row: jobs=%+v err=%v", jobs, err)
	}
}

// recordRebootRunnerEvidence replays the first-turn runner through production
// stores only: claim, start permit, provider identity, lifetime journal.
func recordRebootRunnerEvidence(t *testing.T, fixture *historicalSupervisorFixture, setup rebootSetup) {
	t.Helper()
	td, req := fixture.td, &fixture.req
	if setup.prior != nil {
		if err := td.ClaimSession(req.Provider, setup.prior.ConversationID); err != nil {
			t.Fatal(err)
		}
	}
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	releasePermit := func() {
		if released {
			return
		}
		released = true
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	if setup.holdRunnerLease {
		t.Cleanup(releasePermit)
	} else {
		defer releasePermit()
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if setup.identity {
		identity := task.SessionIdentity{Provider: req.Provider, ConversationID: rebootConversation(setup)}
		if err = td.RecordProviderIdentity(identity); err != nil {
			t.Fatal(err)
		}
	}
	if setup.seal {
		sealRebootTurn(t, td)
	}
	recordRebootLifetime(t, td, setup.stage)
}

func sealRebootTurn(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	if err := td.WriteRawFiles("answer\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
}

func rebootConversation(setup rebootSetup) string {
	if setup.prior != nil {
		return setup.prior.ConversationID
	}
	return "reboot-first-turn"
}

func recordRebootLifetime(t *testing.T, td *taskdir.TaskDir, stage rebootLifetimeStage) {
	t.Helper()
	if stage == rebootLifetimeNone {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	owner, err := td.AcquireLifetimeOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	deadline := time.Now().UTC().Add(time.Hour)
	if _, err = td.RecordLifetimeIntentContext(ctx, taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact, ScopePlan: "reboot-plan", BootID: rebootPreviousBootID, Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	if _, err = td.RecordLifetimeRegistrationContext(ctx, taskdir.LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact, ScopeID: "reboot-scope", BootID: rebootPreviousBootID, Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	if _, err = td.RecordLifetimeWorkerContext(ctx, taskdir.LifetimeWorkerFacts{PID: rebootWorkerPID, ProcessStart: "reboot-worker-start", BootID: rebootPreviousBootID}); err != nil {
		t.Fatal(err)
	}
	switch stage {
	case rebootLifetimeNone, rebootLifetimeOpen:
		// An open lifetime keeps only intent, registration, and worker.
	case rebootLifetimeClosedPreviousBoot:
		if _, err = td.RecordLifetimeTerminationContext(ctx, task.LifetimeTerminationPreviousBoot); err != nil {
			t.Fatal(err)
		}
		if _, err = td.RecordLifetimeWorkerAbsenceContext(ctx, task.LifetimeAbsencePreviousBoot, rebootCurrentBootID); err != nil {
			t.Fatal(err)
		}
	case rebootLifetimeClosedBudget:
		recordNativeLifetimeClosure(t, td, task.LifetimeTerminationBudget)
	case rebootLifetimeClosedRequested:
		recordNativeLifetimeClosure(t, td, task.LifetimeTerminationRequested)
	}
}

// recordNativeLifetimeClosure records the guardian's empty-scope termination
// with cause, then the worker's native Wait that exited with an error.
func recordNativeLifetimeClosure(t *testing.T, td *taskdir.TaskDir, cause string) {
	t.Helper()
	ctx := t.Context()
	if _, err := td.RecordLifetimeTerminationContext(ctx, cause); err != nil {
		t.Fatal(err)
	}
	workerDigest, err := td.LifetimeWorkerDigestContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 1
	wait := taskdir.LifetimeWorkerWaitFacts{WorkerSHA256: workerDigest, ChildPID: rebootWorkerPID, Result: task.LifetimeWorkerWaitExitError, ExitCode: &exitCode}
	if _, err = td.RecordLifetimeWorkerWaitContext(ctx, wait); err != nil {
		t.Fatal(err)
	}
}

// requireRebootClosedPreconditions proves the predecessor really carries the
// reported reboot facts, so a refusal cannot come from a wrong fixture.
func requireRebootClosedPreconditions(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	td := fixture.td
	requireRebootClosedLifetime(t, td)
	requireRebootNoProviderPublication(t, fixture)
	released, err := td.RunnerLeaseReleased()
	if err != nil || !released {
		t.Fatalf("runner lease released=%t err=%v, want released", released, err)
	}
	identity, err := td.ReadProviderIdentity()
	if err != nil {
		t.Fatalf("exact provider identity unreadable: %v", err)
	}
	prior, err := tasklifecycle.ContinuationPrior(td, &fixture.req)
	if err != nil || prior.Provider != identity.Provider || prior.ConversationID != identity.ConversationID || prior.PredecessorTaskID != fixture.req.TaskID {
		t.Fatalf("continuation prior = %+v err=%v", prior, err)
	}
}

// requireRebootClosedLifetime resolves the canonical closure from the loaded
// production records: previous-boot empty scope plus a non-native absence.
func requireRebootClosedLifetime(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), td)
	if err != nil {
		t.Fatalf("canonical lifetime evidence did not load: %v", err)
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil || !closure.Closed() || closure.Kind != task.LifetimeClosureRegisteredRecovered {
		t.Fatalf("lifetime closure = %+v err=%v, want closed registered-recovered", closure, err)
	}
	if evidence.Termination == nil || evidence.Termination.Cause != task.LifetimeTerminationPreviousBoot || !evidence.Termination.ScopeEmpty {
		t.Fatalf("termination = %+v, want previous-boot with empty scope", evidence.Termination)
	}
	wait := evidence.WorkerWait
	if wait == nil || wait.Result != task.LifetimeWorkerWaitAbsent || wait.AbsenceProof != task.LifetimeAbsencePreviousBoot || wait.ExitCode != nil {
		t.Fatalf("worker completion = %+v, want previous-boot absence and no native Wait", wait)
	}
}

// requireRebootNoProviderPublication proves the provider never published an
// outcome, seal, or stop request.
func requireRebootNoProviderPublication(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	for _, name := range []string{"outcome.json", "provider.exit", "stop"} {
		if _, err := os.Stat(filepath.Join(fixture.td.Dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s exists or is unreadable: %v", name, err)
		}
	}
	stops, err := fixture.td.ReadStopRecords()
	if err != nil || len(stops) != 0 {
		t.Fatalf("stop records = %+v err=%v, want none", stops, err)
	}
	inspection, err := fixture.td.Inspect()
	if err != nil || inspection.Outcome != nil {
		t.Fatalf("inspection outcome = %+v err=%v, want none", inspection.Outcome, err)
	}
}

// snapshotRebootTaskFiles captures every regular task file except locks.
func snapshotRebootTaskFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() || strings.HasSuffix(path, ".lock") {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		files[path] = data
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// requireRebootTaskFilesUnchanged fails when any pre-existing file changed or
// disappeared. A fix may add its own handoff records; it may not rewrite them.
func requireRebootTaskFilesUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("existing task record changed: %s err=%v", path, err)
		}
	}
}

// rebootContinuationAdmission builds the successor request and predecessor
// options for a direct tasklifecycle.ResolvePredecessor call.
func rebootContinuationAdmission(t *testing.T, fixture *rebootFixture) (task.TaskRecord, tasklifecycle.PredecessorOptions) {
	t.Helper()
	request, err := admission.BuildRequest(fixture.store.RootID, strings.Repeat("7", 32), fixture.req.Provider, fixture.req.CanonicalCwd, fixture.req.RequestedConfig, []byte("continue"), nil)
	if err != nil {
		t.Fatal(err)
	}
	deps := rebootDependencies(t, fixture).normalized()
	return request, tasklifecycle.PredecessorOptions{Admission: staticAdmissionOptions(deps.withCapturedEnvironment()), Supervisor: supervisorAdmissionOptions(deps, deps.SupervisorOptions)}
}

func rebootSupervisorOptions(fixture *rebootFixture) tasklifecycle.SupervisorOptions {
	return tasklifecycle.SupervisorOptions{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: fixture.clientA}, OpenStore: openNativeSupervisorStore}
}

// requireRebootSuccessorBusy proves the predecessor session claim is retained.
func requireRebootSuccessorBusy(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	if err := fixture.successor.ClaimSession(fixture.req.Provider, fixture.req.PriorSession.ConversationID); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("successor claim = %v, want %v", err, task.ErrSessionBusy)
	}
}
