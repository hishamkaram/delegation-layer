package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// A closed native lifetime is interruption handoff proof only when the turn
// published nothing. These tests add publication evidence to the reboot
// fixture's production stores and drive the real continue, status, collect,
// and predecessor-resolution consumers.

func sealedRebootSetup() rebootSetup {
	return rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true, seal: true}
}

// requireSealedUncollectedReboot proves the predecessor carries a valid seal,
// no outcome, a closed lifetime, and a released runner lease.
func requireSealedUncollectedReboot(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	requireRebootClosedLifetime(t, fixture.td)
	inspection, err := fixture.td.Inspect()
	if err != nil || !inspection.SealExists || inspection.OutcomeExists || inspection.Publication != task.PublicationPending {
		t.Fatalf("inspection = %+v err=%v, want valid seal without outcome", inspection, err)
	}
	released, err := fixture.td.RunnerLeaseReleased()
	if err != nil || !released {
		t.Fatalf("runner lease released=%t err=%v, want released", released, err)
	}
	requireRebootSuccessorBusy(t, fixture)
}

func rebootSessionReleasePath(fixture *rebootFixture) string {
	session := task.ComputeSHA256([]byte(fixture.req.Provider + ":" + fixture.req.PriorSession.ConversationID))
	return filepath.Join(fixture.store.Root, "sessions", session, fixture.req.TaskID+".release.json")
}

func requireNoRebootSessionRelease(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	if _, err := os.Lstat(rebootSessionReleasePath(fixture)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("session release exists or is unreadable: %v", err)
	}
	requireRebootSuccessorBusy(t, fixture)
}

// requireRebootReleaseDigest reads the durable release record and requires the
// named handoff proof, then proves the successor may claim the session.
func requireRebootReleaseDigest(t *testing.T, fixture *rebootFixture, want string) {
	t.Helper()
	data, err := os.ReadFile(rebootSessionReleasePath(fixture))
	if err != nil {
		t.Fatalf("session release unreadable: %v", err)
	}
	var release task.SessionReleaseRecord
	if err = json.Unmarshal(data, &release); err != nil {
		t.Fatal(err)
	}
	if err = task.ValidateSessionReleaseRecord(&release); err != nil || release.PredecessorEvidenceSHA256 != want {
		t.Fatalf("session release = %+v err=%v, want evidence %s", release, err, want)
	}
	if err = fixture.successor.ClaimSession(fixture.req.Provider, fixture.req.PriorSession.ConversationID); err != nil {
		t.Fatalf("successor could not claim the released session: %v", err)
	}
}

func rebootContinue(t *testing.T, fixture *rebootFixture) (commandResult, Response) {
	t.Helper()
	result := continueTask(Arguments{Root: fixture.root, TaskID: fixture.req.TaskID}, rebootDependencies(t, fixture))
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "continue", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return result, decoded
}

// requireOutcomeUnavailable checks the existing refusal identity: the
// predecessor has sealed evidence but no authoritative terminal outcome.
func requireOutcomeUnavailable(t *testing.T, consumer string, err error) {
	t.Helper()
	if err == nil || errors.Is(err, task.ErrSessionBusy) || !strings.Contains(err.Error(), "no authoritative terminal outcome") {
		t.Fatalf("%s = %v, want outcome-unavailable refusal", consumer, err)
	}
}

// requireRebootContinueRefused runs the public continue command and requires
// the existing resolve-predecessor refusal envelope with no successor task.
func requireRebootContinueRefused(t *testing.T, fixture *rebootFixture, state string) {
	t.Helper()
	result, decoded := rebootContinue(t, fixture)
	if result.code != 1 || decoded.TaskRecord != "not_created" || decoded.Failure == nil || decoded.Failure.Stage != "resolve-predecessor" || decoded.Failure.Code != "continuation_unavailable" || decoded.Failure.NextAction != "stop_and_report" {
		t.Fatalf("continue over %s = code %d err=%v %+v", state, result.code, result.err, decoded)
	}
	requireOutcomeUnavailable(t, "continue", result.err)
	if _, err := os.Lstat(filepath.Join(filepath.Dir(fixture.td.Dir), decoded.TaskID)); decoded.TaskID == "" || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("refused continue left successor %q: %v", decoded.TaskID, err)
	}
}

// requireRebootResolutionRefused calls ResolvePredecessor directly and
// requires the outcome-unavailable refusal without a prior session.
func requireRebootResolutionRefused(t *testing.T, fixture *rebootFixture, state string) {
	t.Helper()
	request, options := rebootContinuationAdmission(t, fixture)
	prior, err := tasklifecycle.ResolvePredecessor(fixture.store, options, request, fixture.req.TaskID)
	if prior != nil {
		t.Fatalf("ResolvePredecessor returned prior %+v for %s", prior, state)
	}
	requireOutcomeUnavailable(t, "ResolvePredecessor", err)
}

// collectSealedRebootOutcome publishes the sealed predecessor through the
// public collect command and returns the durable outcome it committed.
func collectSealedRebootOutcome(t *testing.T, fixture *rebootFixture) *task.OutcomeRecord {
	t.Helper()
	// The fixture predicate is in the default registry, as for any sealed task.
	collected := collect(Arguments{Root: fixture.root, TaskID: fixture.req.TaskID}, storeDependencies{})
	if collected.code != 0 || collected.response.Publication != task.PublicationCommitted.String() || collected.response.Outcome == nil {
		t.Fatalf("collect sealed predecessor = code %d err=%v %+v", collected.code, collected.err, collected.response)
	}
	inspection, err := fixture.td.Inspect()
	if err != nil || inspection.Outcome == nil {
		t.Fatalf("collected outcome = %+v err=%v", inspection, err)
	}
	return inspection.Outcome
}

// requireTerminalRebootContinuation requires ResolvePredecessor to admit the
// collected predecessor and return its session.
func requireTerminalRebootContinuation(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	request, options := rebootContinuationAdmission(t, fixture)
	prior, err := tasklifecycle.ResolvePredecessor(fixture.store, options, request, fixture.req.TaskID)
	if err != nil || prior.ConversationID != fixture.req.PriorSession.ConversationID || prior.PredecessorTaskID != fixture.req.TaskID {
		t.Fatalf("terminal continuation after collect = %+v err=%v", prior, err)
	}
}

func TestContinueCollectsSealedRebootPredecessorFirst(t *testing.T) {
	t.Parallel()
	fixture := newRebootFixture(t, sealedRebootSetup())
	requireSealedUncollectedReboot(t, fixture)
	before := snapshotRebootTaskFiles(t, fixture.td.Dir)

	requireRebootContinueRefused(t, fixture, "sealed uncollected predecessor")
	requireNoRebootSessionRelease(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)

	outcome := collectSealedRebootOutcome(t, fixture)
	requireTerminalRebootContinuation(t, fixture)
	requireRebootReleaseDigest(t, fixture, outcome.EvidenceSHA256)
}

func TestPredecessorResolutionRefusesSealedRebootPredecessor(t *testing.T) {
	t.Parallel()
	t.Run("predecessor terminated", func(t *testing.T) {
		fixture := newRebootFixture(t, sealedRebootSetup())
		requireSealedUncollectedReboot(t, fixture)
		before := snapshotRebootTaskFiles(t, fixture.td.Dir)
		requireOutcomeUnavailable(t, "PredecessorTerminated", tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)))
		requireNoRebootSessionRelease(t, fixture)
		requireRebootTaskFilesUnchanged(t, before)
	})
	t.Run("resolve predecessor", func(t *testing.T) {
		fixture := newRebootFixture(t, sealedRebootSetup())
		requireSealedUncollectedReboot(t, fixture)
		before := snapshotRebootTaskFiles(t, fixture.td.Dir)
		requireRebootResolutionRefused(t, fixture, "a sealed uncollected predecessor")
		requireNoRebootSessionRelease(t, fixture)
		requireRebootTaskFilesUnchanged(t, before)
	})
}

// This is a defensive orphan-payload fault state, not a normal provider
// sequence: result.txt exists without provider.exit or outcome.json. Inspect
// reports it as pending publication, so it is never an interruption. Without a
// seal, collect cannot publish it either; every continuation consumer refuses.
func TestOrphanPayloadRebootPredecessorIsNeverHandedOff(t *testing.T) {
	t.Parallel()
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	writeRebootTaskFile(t, fixture, "result.txt", []byte("answer\n"))
	requireOrphanPayloadReboot(t, fixture)
	before := snapshotRebootTaskFiles(t, fixture.td.Dir)

	offer, err := tasklifecycle.ObserveTimeoutContinuation(fixture.root, fixture.td, &fixture.req, &fixture.meta, rebootDependencies(t, fixture).Catalog)
	if err != nil || offer != nil {
		t.Fatalf("orphan payload observation offer = %+v err=%v, want none", offer, err)
	}
	requireOutcomeUnavailable(t, "PredecessorTerminated", tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)))
	requireRebootResolutionRefused(t, fixture, "an orphan payload")
	requireRebootContinueRefused(t, fixture, "orphan payload")
	requireNoRebootSessionRelease(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)
}

// requireOrphanPayloadReboot proves the predecessor carries a pending payload
// without a seal or outcome, a closed lifetime, and a released runner lease.
func requireOrphanPayloadReboot(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	requireRebootClosedLifetime(t, fixture.td)
	inspection, err := fixture.td.Inspect()
	if err != nil || inspection.Publication != task.PublicationPending || inspection.SealExists || inspection.OutcomeExists || inspection.PayloadBasename != "result.txt" {
		t.Fatalf("inspection = %+v err=%v, want pending orphan payload", inspection, err)
	}
	released, err := fixture.td.RunnerLeaseReleased()
	if err != nil || !released {
		t.Fatalf("runner lease released=%t err=%v, want released", released, err)
	}
	requireRebootSuccessorBusy(t, fixture)
}

// Observation keeps the interruption offer for unpublished turns only.
func TestStatusOffersInterruptionOnlyWithoutPublication(t *testing.T) {
	t.Parallel()
	sealed := newRebootFixture(t, sealedRebootSetup())
	observed := rebootStatus(t, sealed)
	if observed.code != 0 || observed.err != nil || observed.response.Status == "interrupted" || observed.response.Continuation != nil || observed.response.Failure != nil {
		t.Fatalf("status over sealed predecessor = code %d err=%v %+v", observed.code, observed.err, observed.response)
	}
	offer, err := tasklifecycle.ObserveTimeoutContinuation(sealed.root, sealed.td, &sealed.req, &sealed.meta, rebootDependencies(t, sealed).Catalog)
	if err != nil || offer != nil {
		t.Fatalf("sealed observation offer = %+v err=%v, want none", offer, err)
	}
	requireNoRebootSessionRelease(t, sealed)

	unsealed := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	offer, err = tasklifecycle.ObserveTimeoutContinuation(unsealed.root, unsealed.td, &unsealed.req, &unsealed.meta, rebootDependencies(t, unsealed).Catalog)
	if err != nil || offer == nil || offer.Cause != tasklifecycle.ContinuationInterrupted || !offer.Resumable {
		t.Fatalf("unsealed observation offer = %+v err=%v, want resumable interruption", offer, err)
	}
}

// The unsealed interruption handoff still releases with the preferred
// closure witness: the empty-scope termination record.
func TestRebootInterruptionReleasesWithClosureWitness(t *testing.T) {
	t.Parallel()
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot})
	requireRebootClosedPreconditions(t, fixture)
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), fixture.td)
	if err != nil {
		t.Fatal(err)
	}
	if err = tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
		t.Fatalf("unsealed closed predecessor was refused: %v", err)
	}
	requireRebootReleaseDigest(t, fixture, canonicalRebootDigest(t, evidence.Termination))
}

func TestHeldRunnerLeaseBlocksRebootInterruption(t *testing.T) {
	t.Parallel()
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true, holdRunnerLease: true})
	before := snapshotRebootTaskFiles(t, fixture.td.Dir)
	if err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("held runner lease = %v, want %v", err, task.ErrSessionBusy)
	}
	offer, err := tasklifecycle.ObserveTimeoutContinuation(fixture.root, fixture.td, &fixture.req, &fixture.meta, rebootDependencies(t, fixture).Catalog)
	if err != nil || offer != nil {
		t.Fatalf("held runner lease observation offer = %+v err=%v, want none", offer, err)
	}
	result, decoded := rebootContinue(t, fixture)
	if result.code != 1 || !errors.Is(result.err, task.ErrSessionBusy) || decoded.TaskRecord != "not_created" || decoded.Failure == nil || decoded.Failure.Stage != "resolve-predecessor" || decoded.Failure.Code != "continuation_unavailable" {
		t.Fatalf("continue with held runner lease = code %d err=%v %+v", result.code, result.err, decoded)
	}
	requireNoRebootSessionRelease(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)
}

// A valid budget stop is checked first and keeps its own handoff digest even
// when the lifetime also closed.
func TestBudgetStopHandoffPrecedesClosedLifetime(t *testing.T) {
	t.Parallel()
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot})
	recordRebootStop(t, fixture, "budget", "budget")
	records, err := fixture.td.ReadStopRecords()
	if err != nil || len(records) != 1 || records[0].Observation == nil || !records[0].Observation.Terminated {
		t.Fatalf("budget stop records = %+v err=%v", records, err)
	}
	if err = tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
		t.Fatalf("budget-stopped closed predecessor was refused: %v", err)
	}
	requireRebootReleaseDigest(t, fixture, canonicalRebootDigest(t, records[0].Observation))
}

// Malformed evidence surfaces its concrete fault in observation and admission
// and never hands off the session. A sealed turn is never an interruption, yet
// its malformed closure still fails reconciliation explicitly.
func TestMalformedRebootEvidenceIsReportedNotHandedOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seal bool
		file string
		want error
	}{
		{name: "malformed seal", file: "provider.exit", want: task.ErrEvidenceFault},
		{name: "malformed outcome", file: "outcome.json", want: task.ErrInvariantFault},
		{name: "malformed closure", file: filepath.Join("lifetime", "terminated.json"), want: task.ErrEvidenceFault},
		{name: "sealed with malformed closure", seal: true, file: filepath.Join("lifetime", "terminated.json"), want: task.ErrEvidenceFault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true, seal: tc.seal})
			writeRebootTaskFile(t, fixture, tc.file, []byte("{"))
			before := snapshotRebootTaskFiles(t, fixture.td.Dir)
			if observed := rebootStatus(t, fixture); observed.code != 1 || observed.err == nil || observed.response.Status == "interrupted" {
				t.Fatalf("status = code %d err=%v %+v, want concrete fault", observed.code, observed.err, observed.response)
			}
			if err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); !errors.Is(err, tc.want) {
				t.Fatalf("PredecessorTerminated = %v, want %v", err, tc.want)
			}
			result, decoded := rebootContinue(t, fixture)
			if result.code == 0 || result.err == nil || decoded.TaskRecord != "not_created" {
				t.Fatalf("continue = code %d err=%v %+v, want refusal", result.code, result.err, decoded)
			}
			requireNoRebootSessionRelease(t, fixture)
			requireRebootTaskFilesUnchanged(t, before)
		})
	}
}

func canonicalRebootDigest[T *task.LifetimeTerminationRecord | *task.StopObservedRecord](t *testing.T, record T) string {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	return task.ComputeSHA256(data)
}
