package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
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

// sealOutputUpgradeTask records the exact session, publishes the raw stream
// under reference, and returns the collected outcome. It is the single
// terminal-evidence path for both predecessor and successor tasks.
func sealOutputUpgradeTask(t *testing.T, td *taskdir.TaskDir, provider, session string, reference task.PredicateRef, stdout string) *task.OutcomeRecord {
	t.Helper()
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordProviderIdentity(task.SessionIdentity{Provider: provider, ConversationID: session}); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles(stdout, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", reference); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	outcome, cleanupErr, collectErr := td.Collect(reference)
	if cleanupErr != nil || collectErr != nil || outcome == nil {
		t.Fatalf("collect under %+v: outcome=%+v cleanup=%v err=%v", reference, outcome, cleanupErr, collectErr)
	}
	return outcome
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
