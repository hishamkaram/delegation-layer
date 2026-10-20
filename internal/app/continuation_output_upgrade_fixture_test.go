package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

const (
	outputUpgradePredecessorID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	outputUpgradeBuild         = "output-upgrade-test"
)

// outputUpgradeFixture is one hermetic state root holding a sealed, committed
// predecessor admitted under case.source, plus the production catalog and a
// finite external supervisor that records submissions without running a
// provider. Nothing here launches a provider, installs pueue, or signals a
// process.
type outputUpgradeFixture struct {
	testCase   outputUpgradeCase
	root       string
	workspace  string
	runner     string
	session    string
	supervisor *pueue.Client
	commandLog string
	deps       Dependencies
	request    task.TaskRecord
	meta       task.MetaRecord
}

func newOutputUpgradeFixture(t *testing.T, testCase outputUpgradeCase) *outputUpgradeFixture {
	t.Helper()
	// Claude's native writable tree includes the system temp directory, so the
	// state root lives under the caller's home, as in the CCR continuation tests.
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	fixture := &outputUpgradeFixture{
		testCase:  testCase,
		root:      filepath.Join(mustCCRContinuationTempDir(t, userHome, "delegation-output-upgrade-"), "state"),
		workspace: testsupport.CanonicalTempDir(t),
		runner:    filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run"),
	}
	// The supervisor captures its command environment when bound, so it is
	// created before the provider-only HOME and PATH replace the caller's.
	var options pueue.Options
	fixture.supervisor, options, _, fixture.commandLog = newOrdinarySubmissionReceiptSupervisor(t)
	t.Cleanup(func() {
		if err := fixture.supervisor.Close(); err != nil {
			t.Error(err)
		}
	})
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	installOutputUpgradeProvider(t, testCase.executable())
	fixture.deps = Dependencies{
		Catalog:                     testCase.catalog(t),
		SupervisorOptions:           options,
		InitialSupervisorExecutable: fixture.supervisor.Binding().ClientExecutable,
		RunnerExecutable:            fixture.runner,
	}
	fixture.createPredecessor(t)
	return fixture
}

// installOutputUpgradeProvider places an inert provider CLI as the only PATH
// entry so the production preparation resolves a stable executable identity.
func installOutputUpgradeProvider(t *testing.T, executable string) {
	t.Helper()
	home := testsupport.CanonicalTempDir(t)
	if err := os.WriteFile(filepath.Join(home, executable), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", home)
}

func (f *outputUpgradeFixture) createPredecessor(t *testing.T) {
	t.Helper()
	store, err := taskdir.InitStoreWithPredicates(f.root, f.deps.Catalog.Registry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	brief := []byte("principal brief")
	request, err := admission.BuildRequest(store.RootID, outputUpgradePredecessorID, f.testCase.provider, f.workspace, f.testCase.requestedConfig(), brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.request = request
	profile := f.testCase.predecessorProfile(t, f)
	if !profile.Plan.Predicate.Equal(f.testCase.source) {
		t.Fatalf("predecessor was not admitted under its exact source %+v: %+v", f.testCase.source, profile.Plan.Predicate)
	}
	meta := tasklifecycle.NewMeta(request, profile, f.supervisor.Binding(), f.runner, task.RunnerOwnershipManaged, outputUpgradeBuild)
	f.meta = meta
	td, err := store.CreateTask(outputUpgradePredecessorID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	f.session, err = f.testCase.freshSession(store.RootID, outputUpgradePredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	outcome := sealOutputUpgradeTask(t, td, f.testCase.provider, f.session, f.testCase.source, f.testCase.predecessorStream(f.session))
	if outcome.Verdict != task.VerdictCommitted {
		t.Fatalf("predecessor under %+v was not committed: %+v", f.testCase.source, outcome)
	}
}

// sealOutputUpgradeTask records the exact session itself, publishes the raw
// stream under reference, and returns the collected outcome. It is the
// historical fixture's provider turn on the canonical seal path.
func sealOutputUpgradeTask(t *testing.T, td *taskdir.TaskDir, provider, session string, reference task.PredicateRef, stdout string) *task.OutcomeRecord {
	t.Helper()
	return sealOutputUpgradeTurn(t, td, reference, stdout, func() error {
		return td.RecordProviderIdentity(task.SessionIdentity{Provider: provider, ConversationID: session})
	})
}

// sealOutputUpgradeTurn is the single terminal-evidence path for fixture
// tasks, predecessor and successor alike: turn runs inside the consumed start
// after the start is recorded, as a provider turn does, and may record the
// session; its stdout is then sealed under reference and collected.
func sealOutputUpgradeTurn(t *testing.T, td *taskdir.TaskDir, reference task.PredicateRef, stdout string, turn func() error) *task.OutcomeRecord {
	t.Helper()
	outcome, err := sealFixtureEvidence(td, reference, stdout, turn)
	if err != nil {
		t.Fatalf("seal and collect under %+v: %v", reference, err)
	}
	return outcome
}

// sealFixtureEvidence owns the start lease for one fixture turn: PrepareStart,
// Consume, RecordStarted, turn, raw stdout, Seal, an explicit Release, then
// Collect. The lease is released on every path; a failure keeps its own error
// joined with the release result.
func sealFixtureEvidence(td *taskdir.TaskDir, reference task.PredicateRef, stdout string, turn func() error) (_ *task.OutcomeRecord, resultErr error) {
	permit, err := td.PrepareStart(0)
	if err != nil {
		return nil, err
	}
	released := false
	defer func() {
		if !released {
			resultErr = errors.Join(resultErr, permit.Release())
		}
	}()
	if err = permit.Consume(); err != nil {
		return nil, err
	}
	if err = td.RecordStarted(0); err != nil {
		return nil, err
	}
	if err = turn(); err != nil {
		return nil, fmt.Errorf("provider turn: %w", err)
	}
	if err = td.WriteRawFiles(stdout, ""); err != nil {
		return nil, err
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", reference); err != nil {
		return nil, err
	}
	released = true
	if err = permit.Release(); err != nil {
		return nil, err
	}
	outcome, cleanupErr, collectErr := td.Collect(reference)
	if err = errors.Join(cleanupErr, collectErr); err != nil {
		return nil, err
	}
	if outcome == nil {
		return nil, errors.New("collection returned no outcome")
	}
	return outcome, nil
}

// TestSealFixtureEvidenceRunsTheTurnInsideTheRecordedStart observes the
// canonical fixture lifecycle through the real store. The turn runs once,
// after the start is recorded and before any seal or outcome, under the held
// lease that can still record the session. A failing turn ends without a seal and
// still releases the start lease, exactly as a completed turn does.
func TestSealFixtureEvidenceRunsTheTurnInsideTheRecordedStart(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("failing turn=%t", failing), func(t *testing.T) {
			requireFixtureTurnLifecycle(t, failing)
		})
	}
}

func requireFixtureTurnLifecycle(t *testing.T, failing bool) {
	t.Helper()
	reference := claude.ReferenceForMode(config.ModeReadOnly)
	store, err := taskdir.InitStoreWithPredicates(filepath.Join(t.TempDir(), "state"), NativeCatalog().Registry())
	if err != nil {
		t.Fatal(err)
	}
	// Each closer is registered the moment its handle is acquired; cleanups
	// run in reverse, so the task closes before its store.
	piConsumerCloser(t, "store", store)
	taskID := strings.Repeat("d", 32)
	td := createProviderRecheckTask(t, store, taskID, providerRecheckSource{provider: claude.Provider, reference: reference})
	piConsumerCloser(t, "task", td)
	session, err := claude.FreshSessionID(store.RootID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	turnErr := errors.New("fixture turn failed")
	turns := 0
	outcome, err := sealFixtureEvidence(td, reference, outputUpgradeCase{provider: claude.Provider, mode: config.ModeReadOnly}.predecessorStream(session), func() error {
		turns++
		if insideErr := requireTurnInsideStart(td); insideErr != nil {
			return insideErr
		}
		if recordErr := td.RecordProviderIdentity(task.SessionIdentity{Provider: claude.Provider, ConversationID: session}); recordErr != nil || failing {
			return errors.Join(recordErr, turnErr)
		}
		return nil
	})
	switch {
	case turns != 1:
		t.Fatalf("turn ran %d times", turns)
	case failing && (!errors.Is(err, turnErr) || outcome != nil):
		t.Fatalf("failing turn outcome=%+v err=%v, want only the turn failure", outcome, err)
	case !failing && (err != nil || outcome == nil || outcome.Verdict != task.VerdictCommitted):
		t.Fatalf("completed turn outcome=%+v err=%v, want committed", outcome, err)
	}
	requireFixtureTurnEnded(t, td, !failing)
}

// requireFixtureTurnEnded requires the start lease released and the seal and
// outcome present exactly when the turn completed.
func requireFixtureTurnEnded(t *testing.T, td *taskdir.TaskDir, sealed bool) {
	t.Helper()
	if inspection, err := td.Inspect(); err != nil || inspection.SealExists != sealed || inspection.OutcomeExists != sealed {
		t.Fatalf("after the turn: inspection=%+v err=%v, want sealed=%t", inspection, err, sealed)
	}
	if released, err := td.RunnerLeaseReleased(); err != nil || !released {
		t.Fatalf("start lease released=%t err=%v after the turn ended", released, err)
	}
}

// requireTurnInsideStart reports a turn that is not running under the held
// start lease after the start was recorded and before any seal or outcome.
func requireTurnInsideStart(td *taskdir.TaskDir) error {
	inspection, err := td.Inspect()
	if err != nil {
		return err
	}
	released, err := td.RunnerLeaseReleased()
	if err != nil {
		return err
	}
	if released || !inspection.StartedExists || inspection.SealExists || inspection.OutcomeExists {
		return errors.New("turn ran outside the held, recorded, unsealed start")
	}
	return nil
}

// outputUpgradeCCRCatalog is the production CCR registration with one harness
// substitution, applied to every preparation hook the registration declares. Real CCR admission runs a supervised launch-help inspection
// that needs a live supervisor and runner, neither of which this hermetic
// harness has. The wrapped candidate therefore keeps the registration's own
// predicate selection and Finalize, feeds it the recorded launch-help
// projection, and exposes the result as the static runtime-only admission.
func outputUpgradeCCRCatalog(t *testing.T, route bool) commonprovider.Catalog {
	t.Helper()
	registration := ccr.Registration()
	prepare, prepareExisting, prepareSuccessor := registration.Prepare, registration.PrepareExisting, registration.PrepareSuccessor
	registration.Prepare = func(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		candidate, err := prepare(request)
		return staticCCRAdmission(t, candidate, err, route)
	}
	registration.PrepareExisting = staticCCRExisting(t, prepareExisting, route)
	if prepareSuccessor != nil {
		registration.PrepareSuccessor = staticCCRExisting(t, prepareSuccessor, route)
	}
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func staticCCRExisting(t *testing.T, prepare commonprovider.PrepareExistingCandidate, route bool) commonprovider.PrepareExistingCandidate {
	t.Helper()
	return func(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
		candidate, err := prepare(request, meta)
		return staticCCRAdmission(t, candidate, err, route)
	}
}

func staticCCRAdmission(t *testing.T, candidate commonprovider.ProfileCandidate, err error, route bool) (commonprovider.ProfileCandidate, error) {
	t.Helper()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	probe := candidate.Inspection
	runtimeOnly := commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: probe.Executable, ExecutableSHA256: probe.ExecutableSHA256,
		Directory: probe.Directory, Environment: probe.Environment, OutputLimit: probe.OutputLimit, Runtime: probe.Runtime,
	}
	snapshot, _, err := runtimeOnly.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	native, err := task.MarshalCanonical(map[string]bool{"resume_route": route})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: probe.Executable, SHA256: probe.ExecutableSHA256, Version: "fixture"}, native)
	if err != nil {
		t.Fatal(err)
	}
	finalize := candidate.Finalize
	candidate.Inspection = &snapshot
	candidate.Admit = func() (commonprovider.PreparedProfile, error) {
		profile, finalizeErr := finalize(facts, time.Now())
		// Static admission claims no observed version; the runner's own
		// preflight supplies it before launch.
		profile.RuntimePreflightSHA256, profile.ObservedVersion = capability, ""
		return profile, finalizeErr
	}
	return candidate, nil
}
