package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSettlementCommandAcceptsExactPrepareAndConfirm(t *testing.T) {
	for _, argv := range [][]string{
		{"settle", waitFirstID, "--prepare", "--json"},
		{"settle", waitFirstID, "--confirm", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--json"},
	} {
		if _, err := ParseArguments(argv); err != nil {
			t.Fatalf("exact maintenance-only settlement command refused: %v", err)
		}
	}
}

func recordSettlementLifetime(t *testing.T, td *taskdir.TaskDir, placement *task.LifetimeCustodyPlacement) {
	t.Helper()
	boot, err := lifetime.BootID()
	requireSettlementTest(t, err)
	self, err := lifetime.ObserveProcess(os.Getpid())
	requireSettlementTest(t, err)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	requireSettlementTest(t, err)
	defer func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	deadline := time.Now().UTC().Add(time.Hour)
	_, err = td.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "pg:" + td.TaskID, BootID: boot, Deadline: deadline})
	requireSettlementTest(t, err)
	_, err = td.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopeID: "pg:" + td.TaskID, BootID: boot, Deadline: deadline, CustodyPlacement: placement})
	requireSettlementTest(t, err)
	_, err = td.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{PID: 1 << 30, ProcessStart: self.StartToken(), BootID: boot})
	requireSettlementTest(t, err)
	_, err = td.RecordLifetimeLaunchContext(t.Context(), taskdir.LifetimeLaunchFacts{Sequence: 1, PID: 1<<30 + 1, ProcessStart: self.StartToken(), PGID: 1<<30 + 1})
	requireSettlementTest(t, err)
}

func confirmSettlementForTest(t *testing.T, td *taskdir.TaskDir) *task.OperatorDispositionRecord {
	t.Helper()
	receipt, err := td.ReadSupervisorReceipt()
	requireSettlementTest(t, err)
	queue := task.OperatorDispositionQueue{NumericTaskID: receipt.NumericTaskID, Label: receipt.Label, Ended: true}
	_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	requireSettlementTest(t, err)
	record, err := td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
	requireSettlementTest(t, err)
	return record
}

func settledObservationFixture(t *testing.T) (*taskdir.Store, *taskdir.TaskDir) {
	t.Helper()
	store, td, _ := newAppTestTask(t, false)
	prepareSettlementAdmission(t, td)
	recordSettlementLifetime(t, td, nil)
	confirmSettlementForTest(t, td)
	return store, td
}

func TestSettlementPublicConsumersStopWithoutReadyOrNativeClosure(t *testing.T) {
	store, td := settledObservationFixture(t)
	commands := map[string]func() commandResult{
		"status": func() commandResult {
			return status(Arguments{Root: store.Root, TaskID: td.TaskID}, Dependencies{ReportDiagnostic: func(error) {}})
		},
		"collect": func() commandResult {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			return collectContext(ctx, Arguments{Root: store.Root, TaskID: td.TaskID, Watch: time.Hour}, storeDependencies{reportDiagnostic: func(error) {}})
		},
		"wait": func() commandResult {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			return waitTasksContext(ctx, Arguments{Root: store.Root, WaitTaskIDs: []string{td.TaskID}, Watch: time.Hour}, storeDependencies{})
		},
	}
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			assertAdministrativeSettlementObservation(t, name, run())
		})
	}
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), td)
	requireSettlementTest(t, err)
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil || closure.Closed() {
		t.Fatalf("closure=%+v err=%v", closure, err)
	}
	if _, err = os.Stat(filepath.Join(td.Dir, "provider.started.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("observation replayed provider")
	}
}

func TestSettlementCLIReadOnlyPrepareAndExplicitConfirm(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	writeEndedHistoricalMigrationStatus(t, fixture)
	recordSettlementLifetime(t, fixture.td, nil)
	deps := fixture.dependencies()
	probe := filepath.Join(fixture.root, ".probe")
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte("write support forbidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := supervisorfixture.SnapshotStore(t, fixture.root)
	a := Arguments{Root: fixture.root, TaskID: fixture.td.TaskID, SettlementPrepare: true}
	prepared := settleContext(t.Context(), a, deps)
	if prepared.code != 0 || prepared.response.Settlement == nil || prepared.response.Settlement.AdministrativeState != "prepared" {
		t.Fatalf("read-only prepare: %+v", prepared)
	}
	if !reflect.DeepEqual(before, supervisorfixture.SnapshotStore(t, fixture.root)) {
		t.Fatal("prepare modified storage or ran a filesystem probe")
	}
	a.SettlementPrepare = false
	a.SettlementConfirm = prepared.response.Settlement.Challenge
	blocked := settleContext(t.Context(), a, deps)
	if blocked.code != 1 {
		t.Fatalf("confirm bypassed normal durability support: %+v", blocked)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	confirmed := settleContext(t.Context(), a, deps)
	if confirmed.code != 0 || confirmed.response.Settlement == nil || confirmed.response.Settlement.AdministrativeState != "settled" {
		t.Fatalf("explicit confirmation: %+v", confirmed)
	}
	again := settleContext(t.Context(), a, deps)
	if again.code != 0 || !reflect.DeepEqual(confirmed.response.Settlement, again.response.Settlement) {
		t.Fatalf("confirmation retry changed winner: %+v", again)
	}
}

func TestSettlementArgumentsRejectAmbiguousOrRemoteApproval(t *testing.T) {
	for _, argv := range [][]string{
		{"settle", waitFirstID},
		{"settle", waitFirstID, "--prepare", "--confirm", strings.Repeat("a", 64)},
		{"settle", waitFirstID, "--confirm", ""},
		{"settle", waitFirstID, "--confirm", "bad"},
		{"settle", "remote:" + waitFirstID, "--prepare"},
		{"settle", waitFirstID, waitSecondID, "--prepare"},
	} {
		if _, err := ParseArguments(argv); err == nil {
			t.Fatalf("accepted ambiguous settlement: %q", argv)
		}
	}
}

func TestSettlementLateRealOutcomeAndMalformedAdministrativeHistoryPrecedence(t *testing.T) {
	for _, verdict := range []string{task.VerdictCommitted, task.VerdictRejected} {
		t.Run(verdict, func(t *testing.T) {
			store, td, _ := newAppTestTask(t, false)
			meta := prepareSettlementAdmission(t, td)
			recordSettlementLifetime(t, td, nil)
			confirmSettlementForTest(t, td)
			before, err := os.ReadFile(filepath.Join(td.Dir, "operator-disposition.json"))
			requireSettlementTest(t, err)
			// Late finalization uses the normal runner/store owner without a provider process.
			permit, err := td.PrepareStart(0)
			requireSettlementTest(t, err)
			requireSettlementTest(t, permit.Consume())
			requireSettlementTest(t, td.RecordStarted(0))
			requireSettlementTest(t, td.WriteRawFiles("late real answer\n", ""))
			if _, err = td.Seal(task.InvocationStarted, 0, "", meta.Predicate); err != nil {
				t.Fatal(err)
			}
			requireSettlementTest(t, permit.Release())
			name := "result.txt"
			if verdict == task.VerdictRejected {
				name = "publish.reject"
			}
			outcome, cleanupErr, publishErr := td.PublishCandidateForTest(verdict, name, []byte("late real answer\n"), meta.Predicate)
			if err = errors.Join(cleanupErr, publishErr); err != nil || outcome == nil {
				t.Fatalf("late native evidence: %v", err)
			}
			for _, malformed := range []bool{false, true} {
				assertLateSettlementOutcome(t, store, td, verdict, malformed)
			}
			requireSettlementTest(t, os.WriteFile(filepath.Join(td.Dir, "operator-disposition.json"), before, 0o600))
		})
	}
}

func prepareSettlementAdmission(t *testing.T, td *taskdir.TaskDir) *task.MetaRecord {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	requireSettlementTest(t, err)
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	requireSettlementTest(t, err)
	submit, err := permit.Record()
	requireSettlementTest(t, err)
	requireSettlementTest(t, errors.Join(permit.Consume(), permit.Release()))
	id := int64(7)
	observed := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: id, Binding: submit.Supervisor, Identity: pueue.Identity{RootID: submit.RootID, TaskID: submit.TaskID, SpecSHA256: submit.SpecSHA256, MetaSHA256: submit.MetaSHA256, NumericTaskID: &id}}
	receipt, err := observed.Receipt()
	requireSettlementTest(t, err)
	requireSettlementTest(t, td.RecordSupervisorReceipt(*receipt))
	return meta
}

func TestSettlementHistoricalDrainControlRefusesReusedNumericWrongLabel(t *testing.T) {
	fixture := prepareObservationPhasePublishedInstallation(t)
	current, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || current == fixture.predecessor {
		t.Fatalf("fixture did not install a different generation: %+v %v", current, err)
	}
	writeSavedPrivateCancelStatus(t, fixture.statusPath, "delegate:"+fixture.store.RootID+":"+strings.Repeat("9", 32), pueue.StateQueued)
	pin, err := fixture.store.PinRootContext(t.Context())
	requireSettlementTest(t, err)
	var client *pueue.Client
	err = pueue.WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		if _, beginErr := pueue.BeginPrivateUpgradeWithLock(ctx, pin, pueue.UpgradeOperationRequired, fixture.options); beginErr != nil {
			return beginErr
		}
		var probeErr error
		client, probeErr = pueue.ProbeSavedPrivateMaintenanceWithLock(ctx, fixture.root, fixture.predecessor, fixture.options, func(context.Context) (string, string, error) { return fixture.clientB, fixture.daemonB, nil })
		return probeErr
	})
	if err != nil || client == nil {
		t.Fatalf("connected historical control never reached exact row gate: %v", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	permit, err := fixture.td.PrepareStop(strings.Repeat("8", 32), "user", time.Time{})
	requireSettlementTest(t, err)
	defer func() {
		if closeErr := permit.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	resetHistoricalSupervisorCommandLog(t, fixture)
	result, err := client.Stop(t.Context(), permit)
	if !errors.Is(err, pueue.ErrUnknown) || result.Matched || result.Attempted {
		t.Fatalf("retired task addressed reused numeric row: %+v %v", result, err)
	}
	assertNoSettlementControlMutation(t, observePhaseStatusCommands(fixture)())
}

func requireSettlementTest(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertLateSettlementOutcome(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir, verdict string, malformed bool) {
	t.Helper()

	if malformed {
		requireSettlementTest(t, os.WriteFile(filepath.Join(td.Dir, "operator-disposition.json"), []byte("{malformed"), 0o600))
	}
	result := collect(Arguments{Root: store.Root, TaskID: td.TaskID}, storeDependencies{reportDiagnostic: func(error) {}})
	want := 0
	if verdict == task.VerdictRejected {
		want = 4
	}
	if result.code != want || result.response.Outcome == nil || result.response.Settlement != nil {
		t.Fatalf("outcome lost precedence malformed=%t: %+v", malformed, result)
	}
	reader, readErr := taskdir.OpenReadOnlyStoreContext(t.Context(), store.Root, NativePredicates())
	requireSettlementTest(t, readErr)
	facts, readErr := reader.ReadTaskEvidence(t.Context(), td.TaskID)
	requireSettlementTest(t, reader.Close())
	if readErr != nil || facts.OperatorDisposition != nil || facts.Inspection.Outcome == nil {
		t.Fatalf("read-only outcome hidden: %+v %v", facts, readErr)
	}
}

func TestSettlementGhostStatusStopsExistingLifetimePending(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	writeEndedHistoricalMigrationStatus(t, fixture)
	recordSettlementLifetime(t, fixture.td, nil)
	confirmSettlementForTest(t, fixture.td)
	result := status(Arguments{Root: fixture.root, TaskID: fixture.td.TaskID}, fixture.dependencies())
	if result.code != 0 || result.response.Pending != nil || result.response.Settlement == nil || result.response.Failure == nil || result.response.Failure.Code != "administrative_settlement" {
		t.Fatalf("ended cooperative ghost stayed in lifetime polling: %+v", result)
	}
}

func assertAdministrativeSettlementObservation(t *testing.T, name string, result commandResult) {
	t.Helper()
	expected := 1
	if name == "status" {
		expected = 0
	}
	if result.code != expected || result.response.Failure == nil || result.response.Failure.Code != "administrative_settlement" || result.response.Failure.NextAction != "stop_and_report" {
		t.Fatalf("settlement did not stop public observation: %+v", result)
	}
	if result.response.Publication != "unknown" || result.response.Liveness == "ended" || result.response.Outcome != nil || result.response.Continuation != nil {
		t.Fatalf("administrative permission became native or publication authority: %+v", result.response)
	}
	if name == "wait" && (result.response.Wait.Satisfied || result.response.Wait.Tasks[0].Ready || result.response.Wait.Tasks[0].Settlement == nil) {
		t.Fatal("settlement satisfied wait readiness")
	}
}

func assertNoSettlementControlMutation(t *testing.T, commands [][]string) {
	t.Helper()
	for _, argv := range commands {
		for _, arg := range argv {
			if arg == "remove" || arg == "kill" {
				t.Fatalf("wrong-label row mutated: %q", argv)
			}
		}
	}
}

func TestSettlementMixedCommittedWaitPreservesAnyAllContract(t *testing.T) {
	store, settled := settledObservationFixture(t)
	committed, _ := newAppTaskInStore(t, store, waitFirstID, true, nil)
	winner, cleanupErr, collectErr := committed.Collect(task.FixturePredicateRef())
	requireSettlementTest(t, errors.Join(cleanupErr, collectErr))
	if winner == nil || winner.Verdict != task.VerdictCommitted {
		t.Fatal("fixture did not publish a genuine committed outcome")
	}
	for _, test := range []struct {
		name      string
		ids       []string
		all       bool
		code      int
		satisfied bool
	}{
		{"any settled first", []string{settled.TaskID, committed.TaskID}, false, 0, true},
		{"any committed first", []string{committed.TaskID, settled.TaskID}, false, 0, true},
		{"all settled first", []string{settled.TaskID, committed.TaskID}, true, 1, false},
		{"all committed first", []string{committed.TaskID, settled.TaskID}, true, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := waitTasks(Arguments{Root: store.Root, WaitTaskIDs: test.ids, WaitAll: test.all}, storeDependencies{})
			if result.code != test.code || result.response.Wait == nil || result.response.Wait.Satisfied != test.satisfied {
				t.Fatalf("mixed wait changed mode semantics: %+v", result)
			}
			assertMixedSettlementWaitSummaries(t, result.response.Wait, test.ids, settled.TaskID)
			if test.satisfied {
				if result.response.Failure != nil {
					t.Fatalf("satisfied any-of inherited an unrelated task failure: %+v", result.response.Failure)
				}
			} else if result.response.Failure == nil || result.response.Failure.Code != "administrative_settlement" || result.response.Failure.NextAction != "stop_and_report" {
				t.Fatalf("unmet all-of lost settlement stop guidance: %+v", result.response.Failure)
			}
		})
	}
}

func assertMixedSettlementWaitSummaries(t *testing.T, watch *WaitResponse, ids []string, settledID string) {
	t.Helper()
	if len(watch.Tasks) != len(ids) {
		t.Fatalf("mixed wait dropped task summaries: %+v", watch.Tasks)
	}
	for index, id := range ids {
		item := watch.Tasks[index]
		if item.TaskID != id {
			t.Fatalf("mixed wait changed requested task order: %+v", watch.Tasks)
		}
		assertMixedSettlementWaitTask(t, item, id == settledID)
	}
}

func assertMixedSettlementWaitTask(t *testing.T, item WaitTask, settled bool) {
	t.Helper()
	if settled {
		if item.Ready || item.Settlement == nil || item.Publication != "unknown" {
			t.Fatalf("settled task became genuine result readiness: %+v", item)
		}
		if item.Failure == nil || item.Failure.Code != "administrative_settlement" {
			t.Fatalf("settled task lost its own administrative summary: %+v", item)
		}
		return
	}
	if !item.Ready || item.Settlement != nil || item.Publication != "committed" {
		t.Fatalf("genuine committed task lost readiness: %+v", item)
	}
}

func TestSettlementMixedPendingWaitKeepsAnyUnsettledMember(t *testing.T) {
	store, settled := settledObservationFixture(t)
	pending, _ := newAppTaskInStore(t, store, waitFirstID, true, nil)
	for _, test := range []struct {
		name string
		ids  []string
		all  bool
		code int
	}{
		{"any settled first", []string{settled.TaskID, pending.TaskID}, false, 3},
		{"any pending first", []string{pending.TaskID, settled.TaskID}, false, 3},
		{"all settled first", []string{settled.TaskID, pending.TaskID}, true, 1},
		{"all pending first", []string{pending.TaskID, settled.TaskID}, true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := waitTasks(Arguments{Root: store.Root, WaitTaskIDs: test.ids, WaitAll: test.all}, storeDependencies{})
			if result.code != test.code || result.response.Wait == nil || result.response.Wait.Satisfied {
				t.Fatalf("settlement stopped an eligible remaining member: code=%d wait=%+v error=%v", result.code, result.response.Wait, result.err)
			}
			assertPendingSettlementWaitSummaries(t, result.response.Wait, test.ids, settled.TaskID)
		})
	}
}

func assertPendingSettlementWaitSummaries(t *testing.T, watch *WaitResponse, ids []string, settledID string) {
	t.Helper()
	if len(watch.Tasks) != len(ids) {
		t.Fatalf("mixed pending wait dropped summaries: %+v", watch.Tasks)
	}
	for index, id := range ids {
		item := watch.Tasks[index]
		if item.TaskID != id {
			t.Fatalf("mixed pending wait changed input order: %+v", watch.Tasks)
		}
		if id == settledID {
			assertMixedSettlementWaitTask(t, item, true)
			continue
		}
		if item.Ready || item.Settlement != nil || item.Publication != "pending" {
			t.Fatalf("unpublished sealed member lost its pending state: %+v", item)
		}
	}
}

func TestSettlementAllMembersSettledStopsAny(t *testing.T) {
	store, first := settledObservationFixture(t)
	second, _ := newAppTaskInStore(t, store, waitFirstID, false, nil)
	prepareSettlementAdmission(t, second)
	recordSettlementLifetime(t, second, nil)
	confirmSettlementForTest(t, second)
	result := waitTasks(Arguments{Root: store.Root, WaitTaskIDs: []string{first.TaskID, second.TaskID}}, storeDependencies{})
	if result.code != 1 || result.response.Wait == nil || result.response.Wait.Satisfied {
		t.Fatalf("all-settled any-of kept watching: %+v", result)
	}
	for _, item := range result.response.Wait.Tasks {
		assertMixedSettlementWaitTask(t, item, true)
	}
}

// These immutable snapshots come from the real rooted reader before and after
// canonical publication. The test owns only their delivery order, not Ready.
type settlementPublicationSnapshots struct {
	*taskdir.ReadOnlyStore
	pendingID string
	before    taskdir.TaskEvidence
	after     taskdir.TaskEvidence
	reads     int
}

func (r *settlementPublicationSnapshots) ReadTaskEvidence(ctx context.Context, id string) (taskdir.TaskEvidence, error) {
	if err := ctx.Err(); err != nil {
		return taskdir.TaskEvidence{TaskID: id}, err
	}
	if id != r.pendingID {
		return r.ReadOnlyStore.ReadTaskEvidence(ctx, id)
	}
	r.reads++
	if r.reads == 1 {
		return r.before, nil
	}
	return r.after, nil
}

func TestSettlementAnyFollowsCanonicalPendingPublication(t *testing.T) {
	store, settled := settledObservationFixture(t)
	pending, _ := newAppTaskInStore(t, store, waitFirstID, true, nil)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), store.Root, NativePredicates())
	requireSettlementTest(t, err)
	t.Cleanup(func() { requireSettlementTest(t, reader.Close()) })
	before, err := reader.ReadTaskEvidence(t.Context(), pending.TaskID)
	requireSettlementTest(t, err)
	if before.Inspection == nil || before.Inspection.Outcome != nil || before.Seal == nil {
		t.Fatal("fixture did not start with genuine pending sealed evidence")
	}
	winner, cleanupErr, collectErr := pending.Collect(task.FixturePredicateRef())
	requireSettlementTest(t, errors.Join(cleanupErr, collectErr))
	if winner == nil || winner.Verdict != task.VerdictCommitted {
		t.Fatal("fixture did not canonically commit its pending outcome")
	}
	after, err := reader.ReadTaskEvidence(t.Context(), pending.TaskID)
	requireSettlementTest(t, err)
	snapshots := &settlementPublicationSnapshots{ReadOnlyStore: reader, pendingID: pending.TaskID, before: before, after: after}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ids := []string{settled.TaskID, pending.TaskID}
	result := watchWaitTasks(ctx, Arguments{WaitTaskIDs: ids, Watch: 5 * time.Second}, snapshots, newResponse("wait"), time.Nanosecond)
	if result.code != 0 || result.response.Wait == nil || !result.response.Wait.Satisfied || snapshots.reads < 2 {
		t.Fatalf("any-of stopped before real pending publication: code=%d reads=%d wait=%+v error=%v", result.code, snapshots.reads, result.response.Wait, result.err)
	}
	assertMixedSettlementWaitSummaries(t, result.response.Wait, ids, settled.TaskID)
}
