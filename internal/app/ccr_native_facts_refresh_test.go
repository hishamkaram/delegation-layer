package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// ccrNativeFactsHelp is the CCR launch help without the route-preserving flag.
const ccrNativeFactsHelp = "Usage: --model --auth-mode --print --permission-mode --input-format --output-format --verbose --permission-prompts --session-id --resume --effort"

// ccrNativeFactsFixture is a queued CCR task admitted through the production
// catalog, inspection worker, and proof journal. Its ccr executable reads the
// launch help from a separate file, so the executable bytes stay fixed while
// the advertised route capability changes between admission and launch. It
// also appends every invocation's logical argv to argvPath, so tests compare
// the provider argv itself rather than the verified launcher's wrapper.
type ccrNativeFactsFixture struct {
	*workerSuccessorFixture
	helpPath string
	argvPath string
}

// ccrNativeFactsInvocationEnd terminates one recorded invocation.
const ccrNativeFactsInvocationEnd = "--ccr-fixture-invocation-end--"

func newCCRNativeFactsFixture(t *testing.T, route bool) *ccrNativeFactsFixture {
	t.Helper()
	fixture := &ccrNativeFactsFixture{workerSuccessorFixture: newWorkerSuccessorFixture(t)}
	fixture.helpPath = filepath.Join(fixture.cliDir, "launch-help")
	fixture.argvPath = filepath.Join(fixture.cliDir, "argv-log")
	fixture.advertiseRoute(t, route)
	// Shell builtins only: the provider scope's PATH holds no other tools.
	// "$0" and "$@" are the logical argv the provider itself receives.
	writeRunnerFixture(t, filepath.Join(fixture.cliDir, "ccr"), fmt.Sprintf(
		"#!/bin/sh\n{ printf '%%s\\n' \"$0\" \"$@\"; printf '%%s\\n' %s; } >> %s\ncase \"$1\" in\n  version|--version) printf '%%s\\n' '1.0.0' ;;\n  *) while IFS= read -r line; do printf '%%s\\n' \"$line\"; done < %s ;;\nesac\n",
		ccrNativeFactsInvocationEnd, shellQuote(fixture.argvPath), shellQuote(fixture.helpPath)))
	return fixture
}

// invocations returns every recorded logical argv in order.
func (f *ccrNativeFactsFixture) invocations(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(f.argvPath)
	if err != nil {
		t.Fatal(err)
	}
	var invocations [][]string
	var current []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == ccrNativeFactsInvocationEnd {
			invocations, current = append(invocations, current), nil
			continue
		}
		current = append(current, line)
	}
	if len(current) != 0 {
		t.Fatalf("argv log ends inside an invocation: %q", current)
	}
	return invocations
}

func (f *ccrNativeFactsFixture) advertiseRoute(t *testing.T, route bool) {
	t.Helper()
	help := ccrNativeFactsHelp
	if route {
		help += " --resume-route"
	}
	if err := os.WriteFile(f.helpPath, []byte(help+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// admit records a fresh CCR task exactly as dispatch admits it: the catalog
// candidate, the managed worker's inspection proof, and its finalization.
func (f *ccrNativeFactsFixture) admit(t *testing.T) (task.TaskRecord, task.MetaRecord, []byte) {
	t.Helper()
	brief := []byte("queued brief")
	request, err := admission.BuildRequest(f.store.RootID, strings.Repeat("e", 32), ccr.Provider, f.workspace, task.TaskConfig{
		Model: "recorded-alias", Permission: config.ModeReadOnly, Budget: "1m0s",
	}, brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NativeCatalog().Candidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	f.queueInspection(t, request, candidate)
	if err = f.runWorker(t, request.TaskID, Dependencies{ResourceChecker: fixtureResources}); err != nil {
		t.Fatalf("worker refused the admission inspection: stage=%s", inspectionWorkerFailureStage(err, "unknown"))
	}
	facts := f.proof(t, request, true)
	profile, err := admission.FinalizeCandidate(candidate, request, facts)
	if err != nil {
		t.Fatal(err)
	}
	meta := tasklifecycle.NewMeta(request, profile, f.supervisor, "/task/delegate-run", task.RunnerOwnershipManaged, "app-test")
	td, err := f.store.CreateTask(request.TaskID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	return request, meta, facts
}

// proof reads the stored admission facts, first recording the supervisor's
// worker-success observation when record is set.
func (f *ccrNativeFactsFixture) proof(t *testing.T, request task.TaskRecord, record bool) []byte {
	t.Helper()
	operation, err := admission.LoadInspectionOperationContext(t.Context(), f.store, request)
	if err != nil {
		t.Fatal(err)
	}
	if record {
		err = operation.RecordWorkerSuccessContext(t.Context(), 7)
	}
	var facts []byte
	if err == nil {
		facts, err = operation.ReadProofContext(t.Context())
	}
	if err = errors.Join(err, operation.Close()); err != nil {
		t.Fatal(err)
	}
	return facts
}

// ccrNativeFactsLaunch is one queued start: the reconstructed plan, every
// provider argv the start hook observed, and the sealed result.
type ccrNativeFactsLaunch struct {
	admitted execution.Plan
	launched [][]string
	result   execution.Result
	dir      string
}

// ccrNativeFactsDependencies is the production composition the runner uses.
func ccrNativeFactsDependencies() Dependencies {
	return Dependencies{ResourceChecker: fixtureResources, Environment: os.Environ(), Catalog: NativeCatalog()}
}

// launch starts the queued task through the runner's own reconstruction and
// the fresh supervised preflight composed by preflight.
func (f *ccrNativeFactsFixture) launch(t *testing.T, request task.TaskRecord, preflight Dependencies) ccrNativeFactsLaunch {
	t.Helper()
	td, err := f.store.OpenTask(request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	persistedRequest, persistedMeta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := prepareMatchedProfile(ccrNativeFactsDependencies(), f.store.Root, *persistedRequest, *persistedMeta)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	launch := ccrNativeFactsLaunch{admitted: profile.Plan, dir: td.Dir}
	launch.result = execution.Run(td, permit, profile.Plan, execution.Options{
		PreflightPlan: func(scope execution.PreflightScope, _ execution.Plan) (execution.Plan, error) {
			return freshPreflightProfile(preflight, f.store.Root, *persistedRequest, *persistedMeta, scope)
		},
		Hooks: execution.Hooks{Start: func(command *exec.Cmd) error {
			launch.launched = append(launch.launched, slices.Clone(command.Args))
			return command.Start()
		}},
	})
	result := launch.result
	if err = errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError); err != nil || result.Outcome == nil {
		t.Fatalf("queued start did not seal: outcome=%+v err=%v", result.Outcome, err)
	}
	return launch
}

func readTaskFile(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestQueuedModeledCCRLaunchesAfterGainingRouteCapability admits a modeled
// task on a CCR without --resume-route and launches it after the same
// executable starts advertising the flag. The modeled launch never uses that
// flag, so the queued task launches once with its recorded argv and predicate;
// the admission proof and metadata stay byte-identical.
func TestQueuedModeledCCRLaunchesAfterGainingRouteCapability(t *testing.T) {
	fixture := newCCRNativeFactsFixture(t, false)
	request, meta, facts := fixture.admit(t)
	if meta.Predicate.Version != "ccr-foreground-v7-modeled" {
		t.Fatalf("admission selected %s, want the modeled contract", meta.Predicate.Version)
	}
	metaBefore := readTaskFile(t, filepath.Join(fixture.store.Root, "tasks", request.TaskID), "meta.json")
	fixture.advertiseRoute(t, true)
	// Keep only the invocations made while this queued task starts.
	if err := os.WriteFile(fixture.argvPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	launch := fixture.launch(t, request, ccrNativeFactsDependencies())
	if len(launch.launched) != 1 {
		t.Fatalf("modeled task launches=%d, want 1; outcome=%+v", len(launch.launched), launch.result.Outcome)
	}
	requireOneAdmittedCCRLaunch(t, fixture.invocations(t), append([]string{launch.admitted.Executable}, launch.admitted.Arguments...))
	if !launch.result.Outcome.Predicate.Equal(meta.Predicate) {
		t.Fatalf("sealed predicate %+v, want recorded %+v", launch.result.Outcome.Predicate, meta.Predicate)
	}
	if !bytes.Equal(readTaskFile(t, launch.dir, "meta.json"), metaBefore) {
		t.Fatal("launch rewrote the admission metadata")
	}
	if !bytes.Equal(fixture.proof(t, request, false), facts) {
		t.Fatal("launch rewrote the stored admission facts")
	}
}

// requireOneAdmittedCCRLaunch compares the provider's own logical argv, not
// the verified launcher's wrapper: every recorded invocation is either a
// capability probe or exactly the admitted executable and arguments, the
// admitted launch runs once, keeps its model and never gains --resume-route.
func requireOneAdmittedCCRLaunch(t *testing.T, invocations [][]string, admitted []string) {
	t.Helper()
	model := slices.Index(admitted, "--model")
	if model < 0 || model+1 >= len(admitted) || admitted[model+1] != "recorded-alias" || slices.Contains(admitted, "--resume-route") {
		t.Fatalf("admitted argv %q lost the recorded model or gained --resume-route", admitted)
	}
	executable := admitted[0]
	probes := [][]string{{executable, "version"}, {executable, "launch", "--help"}}
	launches := 0
	for _, invocation := range invocations {
		switch {
		case slices.Equal(invocation, admitted):
			launches++
		case slices.ContainsFunc(probes, func(probe []string) bool { return slices.Equal(invocation, probe) }):
		default:
			t.Fatalf("provider received argv %q, want a probe or the admitted %q", invocation, admitted)
		}
	}
	if launches != 1 {
		t.Fatalf("admitted argv launched %d times in %q, want once", launches, invocations)
	}
}

// TestQueuedRouteCCRRefusesAfterLosingRouteCapability admits a route-capable
// task and launches it after the CCR stops advertising --resume-route. Nothing
// launches, and the sealed refusal names the changed inspection instead of a
// manifest fault; collection keeps its rejected exit code.
func TestQueuedRouteCCRRefusesAfterLosingRouteCapability(t *testing.T) {
	fixture := newCCRNativeFactsFixture(t, true)
	request, meta, _ := fixture.admit(t)
	if meta.Predicate.Version != "ccr-foreground-v7" {
		t.Fatalf("admission selected %s, want the route contract", meta.Predicate.Version)
	}
	fixture.advertiseRoute(t, false)

	launch := fixture.launch(t, request, ccrNativeFactsDependencies())
	outcome := launch.result.Outcome
	if len(launch.launched) != 0 || outcome.Verdict != task.VerdictRejected {
		t.Fatalf("route task launches=%d verdict=%s, want 0/rejected", len(launch.launched), outcome.Verdict)
	}
	collected := fixture.collect(t, request)
	payload := collected.Payload
	if collected.Publication != task.PublicationRejected.String() || collected.Outcome == nil || !collected.Outcome.Predicate.Equal(meta.Predicate) || payload == nil {
		t.Fatalf("collect publication=%s outcome=%+v, want rejected under %+v", collected.Publication, collected.Outcome, meta.Predicate)
	}
	refusal := readTaskFile(t, launch.dir, payload.Basename)
	if int64(len(refusal)) != payload.Length || task.ComputeSHA256(refusal) != payload.SHA256 {
		t.Fatalf("collected descriptor %+v does not identify the sealed refusal", *payload)
	}
	if want := "provider-failed: start_failed: provider inspection facts changed after this task was admitted, so Delegate did not launch it"; string(refusal) != want {
		t.Fatalf("sealed refusal=%q, want %q", refusal, want)
	}
}

// collect runs the public collect command and returns its JSON response,
// which must exit 4 for a rejected publication and carry only the payload
// descriptor, never the payload bytes.
func (f *ccrNativeFactsFixture) collect(t *testing.T, request task.TaskRecord) Response {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"collect", request.TaskID, "--root", f.store.Root, "--json"}, &stdout, &stderr, ccrNativeFactsDependencies())
	if code != 4 || bytes.Contains(stdout.Bytes(), []byte("did not launch it")) {
		t.Fatalf("collect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// relaxedCCRDependencies reconstructs the queued task through the production
// CCR catalog, then replaces its refreshed-facts decision with one that
// accepts any difference and alters each finalized profile with mutate. A
// finite profile factory routes reconstruction through PrepareCandidate.
func relaxedCCRDependencies(meta task.MetaRecord, decisions *int, mutate func(*PreparedProfile)) Dependencies {
	deps := ccrNativeFactsDependencies()
	deps.PrepareProfile = func(task.TaskRecord) (PreparedProfile, error) { return PreparedProfile{}, ErrProfileUnavailable }
	deps.PrepareCandidate = func(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
		candidate, err := NativeCatalog().ExistingCandidate(request, meta, environment)
		if err != nil {
			return commonprovider.ProfileCandidate{}, err
		}
		finalize := candidate.Finalize
		candidate.AcceptRefreshedNativeFacts = func(json.RawMessage, json.RawMessage) error {
			*decisions++
			return nil
		}
		candidate.Finalize = func(facts json.RawMessage, now time.Time) (PreparedProfile, error) {
			profile, finalizeErr := finalize(facts, now)
			if finalizeErr == nil {
				mutate(&profile)
			}
			return profile, finalizeErr
		}
		return candidate, nil
	}
	return deps
}

// TestRefreshedNativeFactsDecisionCannotRelaxRecordedBindings accepts every
// fresh projection through a permissive decision. The unchanged control
// launches once; a finalized profile whose environment, writable roots, or
// policy differs from the record still launches nothing, because acceptance
// covers only the native facts.
func TestRefreshedNativeFactsDecisionCannotRelaxRecordedBindings(t *testing.T) {
	mutations := map[string]func(*PreparedProfile){
		"unchanged control": func(*PreparedProfile) {},
		"environment": func(profile *PreparedProfile) {
			profile.Plan.Environment = append(slices.Clone(profile.Plan.Environment), "CCR_FIXTURE_DRIFT=1")
		},
		"writable roots": func(profile *PreparedProfile) {
			profile.WritableRoots = append(slices.Clone(profile.WritableRoots), filepath.Join(profile.Plan.Directory, "extra"))
		},
		"policy": func(profile *PreparedProfile) {
			policy := *profile.Effective.Policy
			policy.WritableRoots = append(slices.Clone(policy.WritableRoots), filepath.Join(profile.Plan.Directory, "extra"))
			profile.Effective.Policy = &policy
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			fixture := newCCRNativeFactsFixture(t, false)
			request, meta, _ := fixture.admit(t)
			fixture.advertiseRoute(t, true)
			decisions := 0
			launch := fixture.launch(t, request, relaxedCCRDependencies(meta, &decisions, mutate))
			wantLaunches, wantVerdict := 0, task.VerdictRejected
			if name == "unchanged control" {
				wantLaunches = 1
			}
			if decisions == 0 || len(launch.launched) != wantLaunches || (wantLaunches == 0 && launch.result.Outcome.Verdict != wantVerdict) {
				t.Fatalf("decisions=%d launches=%d verdict=%s, want a consulted decision and %d launches",
					decisions, len(launch.launched), launch.result.Outcome.Verdict, wantLaunches)
			}
		})
	}
}

// TestRouteCCRSuccessorWithoutRouteCapabilityReportsTheCause continues a
// route-preserving CCR task on a CLI that no longer advertises
// --resume-route. Admission still refuses with the unavailable-provider
// identity, exit code, and stop guidance; the public envelope adds a fixed
// reason code and detail naming the missing capability.
func TestRouteCCRSuccessorWithoutRouteCapabilityReportsTheCause(t *testing.T) {
	for _, testCase := range outputUpgradeCCRCases(t) {
		if !testCase.route {
			continue
		}
		t.Run(testCase.name, func(t *testing.T) { requireRouteSuccessorCapabilityFailure(t, testCase) })
	}
}

// requireRouteSuccessorCapabilityFailure finalizes a route-preserving
// successor without the route capability and renders the public continue
// envelope for that admission failure.
func requireRouteSuccessorCapabilityFailure(t *testing.T, testCase outputUpgradeCase) {
	t.Helper()
	environment := installOutputUpgradeProvider(t, testCase.executable())
	request := outputUpgradeSuccessorRequest(t, testCase)
	candidate, err := NativeCatalog().SuccessorCandidate(request, task.MetaRecord{Predicate: testCase.source, EnvironmentRecorded: true}, environment)
	if err != nil {
		t.Fatal(err)
	}
	_, err = admission.FinalizeCandidate(candidate, request, outputUpgradeCCRFacts(t, candidate, false))
	err = admission.WithStage("finalize-provider", err)
	if !errors.Is(err, ErrProfileUnavailable) || classifyCode(err, 1) != 2 {
		t.Fatalf("missing route capability error=%v exit=%d, want %v/2", err, classifyCode(err, 1), ErrProfileUnavailable)
	}
	response := newResponse("continue")
	response.TaskRecord = "not_created"
	var output bytes.Buffer
	if err = writeJSONResponse(&output, "continue", &response, err); err != nil {
		t.Fatal(err)
	}
	var public Response
	if err = json.Unmarshal(output.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	failure := public.Failure
	if failure == nil || failure.Code != "provider_unavailable" || failure.Stage != "finalize-provider" ||
		failure.NextAction != "stop_and_report" || failure.ReasonCode != "provider_capability_changed" {
		t.Fatalf("public failure=%+v", failure)
	}
	if want := dispatchFailureMessage("provider_unavailable") + " The installed provider CLI no longer offers a capability that this task's recorded launch requires."; public.Error != want {
		t.Fatalf("public error=%q, want %q", public.Error, want)
	}
}
