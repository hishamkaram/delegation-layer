package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/provider/pi"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// recordingResources returns its scripted results in order, then clear
// assessments, and counts every call. With forbidden set, any call fails the
// test: that command must never sample resources.
type recordingResources struct {
	t           *testing.T
	forbidden   bool
	assessments []func() resourceadmission.Assessment
	scope       resourceadmission.Scope
	err         error
	onCheck     func()
	mu          sync.Mutex
	checks      int
	scopes      int
	order       []string
}

func (r *recordingResources) Check(context.Context, resourceadmission.TargetResolver) (resourceadmission.Assessment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forbidden {
		r.t.Error("resource check invoked by a command that must not sample")
	}
	r.checks++
	r.order = append(r.order, "check")
	if r.onCheck != nil {
		r.onCheck()
	}
	if r.err != nil {
		return resourceadmission.Assessment{}, r.err
	}
	if r.checks <= len(r.assessments) {
		return r.assessments[r.checks-1](), nil
	}
	return testClearAssessment(time.Now()), nil
}

func (r *recordingResources) Scope(context.Context, resourceadmission.TargetResolver) (resourceadmission.Scope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forbidden {
		r.t.Error("resource scope invoked by a command that must not sample")
	}
	r.scopes++
	r.order = append(r.order, "scope")
	return r.scope, nil
}

// sampledOnceBeforeRefresh reports that admission took exactly one check
// before its freshness scope; a later check is only the age-based refresh.
func (r *recordingResources) sampledOnceBeforeRefresh() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.order) >= 2 && r.order[0] == "check" && r.order[1] == "scope" && r.scopes == 1
}

func (r *recordingResources) calls() (checks, scopes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks, r.scopes
}

func testClearAssessment(at time.Time) resourceadmission.Assessment {
	return resourceadmission.NewAssessment(at, resourceadmission.Scope{}, resourceadmission.Findings{})
}

func clearNow() resourceadmission.Assessment { return testClearAssessment(time.Now()) }

func clearStale() resourceadmission.Assessment {
	return testClearAssessment(time.Now().Add(-10 * time.Second))
}

func testUnknownAssessment() resourceadmission.Assessment {
	return resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, resourceadmission.Findings{
		Unavailable: []resourceadmission.Unavailable{{Metric: resourceadmission.MetricHostCPU, Reason: resourceadmission.ReasonDeadline}},
		Advisories:  []resourceadmission.Advisory{resourceadmission.AdvisoryMemoryHighNear},
	})
}

// testPressureAssessment confirms critical host memory while host CPU is
// missing: confirmed pressure must still refuse.
func testPressureAssessment() resourceadmission.Assessment {
	return resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, resourceadmission.Findings{
		Pressures:   []resourceadmission.Pressure{{Metric: resourceadmission.MetricHostMemory, Basis: resourceadmission.BasisCritical}},
		Unavailable: []resourceadmission.Unavailable{{Metric: resourceadmission.MetricHostCPU, Reason: resourceadmission.ReasonDeadline}},
	})
}

func freshPiDispatchArguments(fixture *interactivePiResumeFixture) Arguments {
	return Arguments{
		Root: fixture.root, Provider: pi.Provider, Cwd: fixture.workspace,
		BriefBytes:  []byte("fresh resource-checked Pi task"),
		PueueConfig: fixture.supervisor.Binding().ConfigPath, Runner: fixture.runner,
		Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m", Interactive: true},
	}
}

func newResourcePiFixture(t *testing.T, continueCommand bool, resources *recordingResources) *interactivePiResumeFixture {
	t.Helper()
	fixture := newInteractivePiResumeFixture(t, interactivePiResumeTestCase{name: "resource admission", continueCommand: continueCommand}, errors.New("unused"))
	resources.t = t
	fixture.deps.ResourceChecker = resources
	return fixture
}

func jsonResponseFor(t *testing.T, command string, result commandResult) Response {
	t.Helper()
	var stdout bytes.Buffer
	response := result.response
	if err := writeJSONResponse(&stdout, command, &response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func taskEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "tasks"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func assertResourcePressureFailure(t *testing.T, response Response, taskRecord, nextAction string, retryAfter int) {
	t.Helper()
	failure := response.Failure
	if response.TaskRecord != taskRecord || failure == nil || failure.Code != "resource_pressure" || failure.Stage != "check-resources" ||
		failure.ReasonCode != "host_memory_critical" || failure.NextAction != nextAction || failure.RetryAfterSeconds != retryAfter {
		t.Fatalf("resource refusal guidance = record %q failure %+v", response.TaskRecord, failure)
	}
	if !strings.Contains(response.Error, "resource pressure was confirmed. Pressure: host_memory (critical).") {
		t.Fatalf("resource refusal error = %q", response.Error)
	}
	resource := response.ResourceAdmission
	if resource == nil || resource.Status != ResourceAdmissionPressure || len(resource.Warnings) != 1 ||
		resource.Warnings[0].Code != ResourceObservationUnavailable || resource.Warnings[0].Metric != "host_cpu" || resource.Warnings[0].ReasonCode != "deadline" {
		t.Fatalf("resource refusal metadata = %+v", resource)
	}
}

func TestFreshDispatchProjectsResourceStatusWithoutChangingAdmission(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		initial  func() resourceadmission.Assessment
		status   string
		warnings []string
	}{
		{name: "clear", initial: clearNow, status: ResourceAdmissionClear},
		{name: "unknown", initial: testUnknownAssessment, status: ResourceAdmissionUnknown, warnings: []string{ResourceObservationUnavailable + "/host_cpu/deadline", ResourceAdvisory + "/cgroup_memory/memory_high_headroom_low"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resources := &recordingResources{assessments: []func() resourceadmission.Assessment{testCase.initial}}
			fixture := newResourcePiFixture(t, false, resources)
			result := dispatch(freshPiDispatchArguments(fixture), fixture.deps.normalized())
			if result.code != 0 || result.err != nil || result.response.TaskRecord != "created" || result.response.Admission != task.AdmissionAdmitted.String() {
				t.Fatalf("%s resource status changed admission: %+v", testCase.name, result)
			}
			assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
			if !resources.sampledOnceBeforeRefresh() {
				t.Fatalf("fresh dispatch call order = %v, want one check before the freshness scope", resources.order)
			}
			response := jsonResponseFor(t, "dispatch", result)
			if response.Failure != nil || response.Error != "" || response.ResourceAdmission == nil || response.ResourceAdmission.Status != testCase.status {
				t.Fatalf("resource metadata changed the result: %+v", response)
			}
			var warnings []string
			for _, warning := range response.ResourceAdmission.Warnings {
				warnings = append(warnings, warning.Code+"/"+warning.Metric+"/"+warning.ReasonCode)
			}
			if strings.Join(warnings, ",") != strings.Join(testCase.warnings, ",") {
				t.Fatalf("warnings = %v, want %v", warnings, testCase.warnings)
			}
		})
	}
}

func TestContinueSharesOneResourceCheckWithItsDispatch(t *testing.T) {
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{testUnknownAssessment}}
	fixture := newResourcePiFixture(t, true, resources)
	result := dispatchInteractivePiResume(fixture)
	if result.code != 0 || result.err != nil || result.response.TaskRecord != "created" {
		t.Fatalf("continue with unknown resources was not admitted: %+v", result)
	}
	if !resources.sampledOnceBeforeRefresh() {
		t.Fatalf("continue call order = %v, want its initial check shared with its dispatch", resources.order)
	}
	if resource := result.response.ResourceAdmission; resource == nil || resource.Status != ResourceAdmissionUnknown {
		t.Fatalf("continue lost its resource metadata: %+v", resource)
	}
}

func TestContinuePressureRefusesBeforeSuccessorWork(t *testing.T) {
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{testPressureAssessment}}
	fixture := newResourcePiFixture(t, true, resources)
	result := dispatchInteractivePiResume(fixture)
	if result.code != 1 || !errors.Is(result.err, tasklifecycle.ErrResourcePressure) {
		t.Fatalf("continue under pressure = %+v", result)
	}
	if len(fixture.candidateSessionRoots) != 0 {
		t.Fatalf("continue under pressure prepared provider candidates: %v", fixture.candidateSessionRoots)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
	if entries := taskEntries(t, fixture.root); len(entries) != 1 || entries[0] != fixture.predecessorID {
		t.Fatalf("continue under pressure changed task entries: %v", entries)
	}
	assertResourcePressureFailure(t, jsonResponseFor(t, "continue", result), "not_created", "retry_later", 30)
}

func TestFreshDispatchLateResourceRefreshRefusesBeforeTaskRecord(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		initial func() resourceadmission.Assessment
		scope   resourceadmission.Scope
	}{
		{name: "stale assessment", initial: clearStale},
		{name: "changed execution target", initial: clearNow, scope: resourceadmission.Scope{TargetIdentity: "supervisor:1:changed"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resources := &recordingResources{scope: testCase.scope, assessments: []func() resourceadmission.Assessment{testCase.initial, testPressureAssessment}}
			fixture := newResourcePiFixture(t, false, resources)
			result := dispatch(freshPiDispatchArguments(fixture), fixture.deps.normalized())
			if result.code != 1 || result.response.TaskRecord != "not_created" {
				t.Fatalf("late refusal = %+v", result)
			}
			if checks, scopes := resources.calls(); checks != 2 || scopes != 1 {
				t.Fatalf("late refusal checks=%d scopes=%d, want an initial and a refreshed check", checks, scopes)
			}
			// Provider preparation and inspection already ran; the refusal
			// precedes only the task record, capture and submission.
			if len(fixture.candidateSessionRoots) == 0 {
				t.Fatal("late refusal did not reach provider preparation")
			}
			assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
			if entries := taskEntries(t, fixture.root); len(entries) != 1 || entries[0] != fixture.predecessorID {
				t.Fatalf("late refusal created a task entry: %v", entries)
			}
			assertResourcePressureFailure(t, jsonResponseFor(t, "dispatch", result), "not_created", "retry_later", 30)
		})
	}
}

// An explicit task ID that becomes present while its fresh admission is
// prepared belongs to existing-task recovery: the late refresh reads no
// resource metric and the present entry is preserved.
func TestFreshDispatchEntryThatAppearsTakesNoResourceRefresh(t *testing.T) {
	id := strings.Repeat("9", 32)
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{clearStale}}
	fixture := newResourcePiFixture(t, false, resources)
	accepted := filepath.Join(fixture.root, "tasks", id, "accepted")
	resources.onCheck = func() {
		if err := os.MkdirAll(filepath.Dir(accepted), 0o700); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(accepted, []byte("work"), 0o600); err != nil {
			t.Error(err)
		}
	}
	arguments := freshPiDispatchArguments(fixture)
	arguments.TaskID = id
	result := dispatch(arguments, fixture.deps.normalized())
	if checks, scopes := resources.calls(); checks != 1 || scopes != 0 {
		t.Fatalf("present entry took resource reads: checks=%d scopes=%d result=%+v", checks, scopes, result)
	}
	if errors.Is(result.err, tasklifecycle.ErrResourcePressure) || result.response.ResourceAdmission == nil {
		t.Fatalf("present entry result = %+v", result)
	}
	if data, err := os.ReadFile(accepted); err != nil || string(data) != "work" {
		t.Fatalf("present entry changed: %q %v", data, err)
	}
}

func newEarlyResourceDispatch(t *testing.T, resources *recordingResources) (preflightFixture, Dependencies, []string) {
	t.Helper()
	fixture := preflightFixture{root: filepath.Join(testsupport.CanonicalTempDir(t), "state"), cwd: testsupport.CanonicalTempDir(t)}
	brief := filepath.Join(fixture.cwd, "brief.md")
	writeAppTestFile(t, brief, []byte("must not be prepared under resource pressure"))
	resources.t = t
	deps := Dependencies{ResourceChecker: resources, Environment: os.Environ()}
	args := []string{
		"--root", fixture.root, "dispatch", "--provider", "fixture:preflight", "--cwd", fixture.cwd, "--brief", brief,
		"--isolation", "git-worktree", "--permission", "read-only", "--budget", "1m",
	}
	return fixture, deps, args
}

func assertNoManagedWorkspace(t *testing.T, root string, deps Dependencies) {
	t.Helper()
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if ids, idsErr := workspace.ResourceIDs(t.Context(), store); idsErr != nil || len(ids) != 0 {
		t.Fatalf("pressure prepared a managed workspace: ids=%v err=%v", ids, idsErr)
	}
}

func TestFreshDispatchPressureRefusesBeforeWorkspaceAndProvider(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		extra      []string
		taskRecord string
		nextAction string
		retryAfter int
	}{
		{name: "generated id", taskRecord: "not_created", nextAction: "retry_later", retryAfter: 30},
		{name: "explicit id", extra: []string{"--id", strings.Repeat("f", 32)}, taskRecord: "unknown", nextAction: "stop_and_report"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resources := &recordingResources{assessments: []func() resourceadmission.Assessment{testPressureAssessment}}
			fixture, deps, args := newEarlyResourceDispatch(t, resources)
			deps.Catalog = preflightFixtureCatalog(t, fixture.candidate)
			var stdout, stderr bytes.Buffer
			code := Run(append(append(args, testCase.extra...), "--json"), &stdout, &stderr, deps)
			var response Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("decode: %v stdout=%s stderr=%s", err, &stdout, &stderr)
			}
			if code != 1 || fixture.prepareCalls != 0 {
				t.Fatalf("pressure exit=%d prepare=%d response=%s", code, fixture.prepareCalls, &stdout)
			}
			assertResourcePressureFailure(t, response, testCase.taskRecord, testCase.nextAction, testCase.retryAfter)
			if entries := taskEntries(t, fixture.root); len(entries) != 0 {
				t.Fatalf("pressure created task entries: %v", entries)
			}
			assertNoManagedWorkspace(t, fixture.root, deps)
		})
	}
}

func TestHumanResourcePressureNamesResourceCause(t *testing.T) {
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{testPressureAssessment}}
	fixture, deps, args := newEarlyResourceDispatch(t, resources)
	deps.Catalog = preflightFixtureCatalog(t, fixture.candidate)
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr, deps); code != 1 {
		t.Fatalf("human pressure exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	if !strings.Contains(stdout.String(), "resource_admission=pressure\n") {
		t.Fatalf("human output lacks resource status: %q", stdout.String())
	}
	if want := "Delegate refused new work because resource pressure was confirmed. Pressure: host_memory (critical)."; !strings.Contains(stderr.String(), want) || strings.Contains(stderr.String(), "supervisor") {
		t.Fatalf("human error = %q, want %q", stderr.String(), want)
	}
}

func TestResourcePressureJoinedFaultLosesRetryGuidance(t *testing.T) {
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{testPressureAssessment}}
	fixture, deps, args := newEarlyResourceDispatch(t, resources)
	deps.Catalog = preflightFixtureCatalog(t, fixture.candidate)
	parsed, err := ParseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	refused := dispatch(parsed, deps.normalized())
	wrapped := refused
	wrapped.err = errors.Join(refused.err)
	assertResourcePressureFailure(t, jsonResponseFor(t, "dispatch", wrapped), "not_created", "retry_later", 30)

	closeFault := errors.New("store close failed")
	joined := refused
	mergeCommandClose(&joined, func() error { return closeFault })
	response := jsonResponseFor(t, "dispatch", joined)
	if !errors.Is(joined.err, closeFault) || joined.code != 1 {
		t.Fatalf("close fault lost its identity: %+v", joined)
	}
	if response.Failure == nil || response.Failure.Code == "resource_pressure" || response.Failure.NextAction != "stop_and_report" || response.Failure.RetryAfterSeconds != 0 {
		t.Fatalf("joined close fault kept resource retry guidance: %+v", response.Failure)
	}
	if response.ResourceAdmission == nil || response.ResourceAdmission.Status != ResourceAdmissionPressure {
		t.Fatalf("joined close fault dropped the observation: %+v", response.ResourceAdmission)
	}
}

func TestResourceCheckerCancellationKeepsCallerError(t *testing.T) {
	resources := &recordingResources{err: context.Canceled}
	fixture, deps, args := newEarlyResourceDispatch(t, resources)
	deps.Catalog = preflightFixtureCatalog(t, fixture.candidate)
	parsed, err := ParseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	result := dispatch(parsed, deps.normalized())
	if result.code != 1 || !errors.Is(result.err, context.Canceled) || fixture.prepareCalls != 0 {
		t.Fatalf("checker cancellation = %+v prepare=%d", result, fixture.prepareCalls)
	}
	response := jsonResponseFor(t, "dispatch", result)
	if response.Failure == nil || response.Failure.Code == "resource_pressure" || response.Failure.Stage != "check-resources" ||
		response.Failure.NextAction != "stop_and_report" || response.Failure.RetryAfterSeconds != 0 || response.ResourceAdmission != nil {
		t.Fatalf("cancellation became resource guidance: failure=%+v resource=%+v", response.Failure, response.ResourceAdmission)
	}
}

// Existing tasks, recovery, observation, and static commands never sample.
func TestExistingTaskCommandsNeverCheckResources(t *testing.T) {
	resources := &recordingResources{forbidden: true}
	fixture := newResourcePiFixture(t, false, resources)
	for _, args := range [][]string{
		{"help"},
		{"version"},
		{"--root", fixture.root, "status", fixture.predecessorID, "--json"},
		{"--root", fixture.root, "collect", fixture.predecessorID, "--json"},
		{"--root", fixture.root, "cancel", fixture.predecessorID, "--json"},
	} {
		var stdout, stderr bytes.Buffer
		Run(args, &stdout, &stderr, fixture.deps)
	}
	replay := freshPiDispatchArguments(fixture)
	replay.TaskID = fixture.predecessorID
	result := dispatch(replay, fixture.deps.normalized())
	if result.response.ResourceAdmission != nil {
		t.Fatalf("existing-task dispatch projected a resource check: %+v", result.response.ResourceAdmission)
	}
	if checks, scopes := resources.calls(); checks != 0 || scopes != 0 {
		t.Fatalf("existing-task commands checked resources: checks=%d scopes=%d", checks, scopes)
	}
}
