package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func cpuDiagnosticNumber(value float64) *float64 { return &value }

func cpuDiagnosticFindings() resourceadmission.Findings {
	return resourceadmission.Findings{
		Pressures: []resourceadmission.Pressure{{Metric: resourceadmission.MetricHostCPU, Basis: resourceadmission.BasisSustained}},
		HostCPU: &resourceadmission.HostCPUDiagnostics{
			CounterUnit: resourceadmission.CPUCounterTicks, BusyThreshold: 0.95, RequiredStreak: 3,
			Intervals: []resourceadmission.HostCPUInterval{
				{ElapsedMilliseconds: 1000, BusyDelta: cpuDiagnosticNumber(95), TotalDelta: cpuDiagnosticNumber(100), BusyFraction: cpuDiagnosticNumber(0.95), Classification: resourceadmission.CPUIntervalHot},
				{ElapsedMilliseconds: 1002, BusyDelta: cpuDiagnosticNumber(198), TotalDelta: cpuDiagnosticNumber(200), BusyFraction: cpuDiagnosticNumber(0.99), Classification: resourceadmission.CPUIntervalHot},
				{ElapsedMilliseconds: 1001, BusyDelta: cpuDiagnosticNumber(297), TotalDelta: cpuDiagnosticNumber(300), BusyFraction: cpuDiagnosticNumber(0.99), Classification: resourceadmission.CPUIntervalHot},
			},
		},
	}
}

func cpuPressureNow() resourceadmission.Assessment {
	return resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, cpuDiagnosticFindings())
}

func earlyCPURefusal(t *testing.T) (preflightFixture, Dependencies, commandResult) {
	t.Helper()
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{cpuPressureNow}}
	fixture, deps, args := newEarlyResourceDispatch(t, resources)
	deps.Catalog = preflightFixtureCatalog(t, fixture.candidate)
	arguments, err := ParseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, deps, dispatch(arguments, deps.normalized())
}

func TestResourceCPUDiagnosticsJSONContractAndHumanCompatibility(t *testing.T) {
	const secret = "/private/cpu-secret PID=4242 TOKEN=cpu-secret"
	assessment := resourceadmission.NewAssessment(time.Unix(123456789, 0), resourceadmission.Scope{TargetIdentity: secret, Cgroup: secret}, cpuDiagnosticFindings())
	response := newResponse("dispatch")
	_, _, refused := earlyCPURefusal(t)
	response.ResourceAdmission = resourceAdmissionResponse(&assessment, refused.err)
	data, err := encodeJSONResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	const want = `{"status":"pressure","diagnostics":{"host_cpu":{"counter_unit":"ticks","busy_threshold":0.95,"required_streak":3,"intervals":[{"elapsed_ms":1000,"busy_delta":95,"total_delta":100,"busy_fraction":0.95,"classification":"hot"},{"elapsed_ms":1002,"busy_delta":198,"total_delta":200,"busy_fraction":0.99,"classification":"hot"},{"elapsed_ms":1001,"busy_delta":297,"total_delta":300,"busy_fraction":0.99,"classification":"hot"}]}}}`
	if string(wire["resource_admission"]) != want || string(wire["schema_version"]) != "1" {
		t.Fatalf("resource contract changed: %s schema=%s", wire["resource_admission"], wire["schema_version"])
	}
	for _, forbidden := range []string{secret, "123456789", "observed_at", "TargetIdentity", "Cgroup", "NaN", "Inf"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Errorf("response leaked %q: %s", forbidden, data)
		}
	}
	var decoded Response
	if err = json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded.ResourceAdmission, response.ResourceAdmission) {
		t.Fatalf("diagnostics round trip changed values: %+v err=%v", decoded.ResourceAdmission, err)
	}
	var human bytes.Buffer
	if err = writeHumanResponse(&human, response); err != nil {
		t.Fatal(err)
	}
	withoutDiagnostics := response
	resource := *response.ResourceAdmission
	resource.Diagnostics = nil
	withoutDiagnostics.ResourceAdmission = &resource
	var plainHuman bytes.Buffer
	if err = writeHumanResponse(&plainHuman, withoutDiagnostics); err != nil {
		t.Fatal(err)
	}
	if human.String() != plainHuman.String() {
		t.Fatalf("optional JSON evidence changed human output: %q != %q", human.String(), plainHuman.String())
	}
	truncated := truncateResourceAdmission(response.ResourceAdmission, 0)
	if !reflect.DeepEqual(truncated.Diagnostics, response.ResourceAdmission.Diagnostics) {
		t.Fatal("diagnostic truncation changed bounded structured evidence")
	}
	*response.ResourceAdmission.Diagnostics.HostCPU.Intervals[0].BusyDelta = 0
	if *assessment.HostCPUDiagnostics().Intervals[0].BusyDelta != 95 {
		t.Fatal("projection shared mutable evidence with its authoritative assessment")
	}
}

func TestResourceCPUDiagnosticsOmissionPreservesOtherWireValues(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		findings resourceadmission.Findings
	}{
		{name: "clear"},
		{name: "unknown", findings: resourceadmission.Findings{Unavailable: []resourceadmission.Unavailable{{Metric: resourceadmission.MetricHostCPU, Reason: resourceadmission.ReasonUnreadable}}}},
		{name: "memory refusal", findings: resourceadmission.Findings{Pressures: []resourceadmission.Pressure{{Metric: resourceadmission.MetricHostMemory, Basis: resourceadmission.BasisCritical}}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			withEvidence := testCase.findings
			withEvidence.HostCPU = cpuDiagnosticFindings().HostCPU
			assessment := resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, withEvidence)
			plain := resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, testCase.findings)
			withResponse, plainResponse := newResponse("dispatch"), newResponse("dispatch")
			withResponse.ResourceAdmission, plainResponse.ResourceAdmission = resourceAdmissionResponse(&assessment, nil), resourceAdmissionResponse(&plain, nil)
			actual, err := encodeJSONResponse(withResponse)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := encodeJSONResponse(plainResponse)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, expected) || bytes.Contains(actual, []byte("diagnostics")) {
				t.Fatalf("non-CPU-confirmed wire changed: %s != %s", actual, expected)
			}
		})
	}
}

func TestResourceCPUDiagnosticsOmittedForPressureAssessmentWithoutRefusal(t *testing.T) {
	assessment := cpuPressureNow()
	response := resourceAdmissionResponse(&assessment, nil)
	if response.Status != ResourceAdmissionPressure || response.Diagnostics != nil {
		t.Fatalf("an assessment without a refused command exposed CPU refusal evidence: %+v", response)
	}
}

func assertCPUResourceRefusal(t *testing.T, response Response) {
	t.Helper()
	if response.TaskRecord != "not_created" || response.SchemaVersion != 1 || response.Failure == nil ||
		response.Failure.Code != "resource_pressure" || response.Failure.Stage != "check-resources" ||
		response.Failure.ReasonCode != "host_cpu_sustained" || response.Failure.NextAction != "retry_later" || response.Failure.RetryAfterSeconds != 30 {
		t.Fatalf("CPU evidence changed refusal guidance: record=%s failure=%+v", response.TaskRecord, response.Failure)
	}
	if !strings.Contains(response.Error, "Pressure: host_cpu (sustained).") || response.ResourceAdmission == nil ||
		response.ResourceAdmission.Diagnostics == nil || response.ResourceAdmission.Diagnostics.HostCPU == nil ||
		len(response.ResourceAdmission.Diagnostics.HostCPU.Intervals) != 3 {
		t.Fatalf("CPU refusal lost its cause/evidence: error=%q resource=%+v", response.Error, response.ResourceAdmission)
	}
}

func TestResourceCPUDiagnosticsFollowFreshDispatchRefusalAndJoinedFault(t *testing.T) {
	fixture, deps, result := earlyCPURefusal(t)
	if result.code != 1 || fixture.prepareCalls != 0 || !errors.Is(result.err, tasklifecycle.ErrResourcePressure) {
		t.Fatalf("CPU pressure did not refuse before provider work: %+v prepare=%d", result, fixture.prepareCalls)
	}
	assertCPUResourceRefusal(t, jsonResponseFor(t, "dispatch", result))
	if entries := taskEntries(t, fixture.root); len(entries) != 0 {
		t.Fatalf("CPU refusal created task records: %v", entries)
	}
	closeFault := errors.New("close secret must stay out of CPU evidence")
	assessment := cpuPressureNow()
	joined := dispatchCommandResult(tasklifecycle.DispatchResult{
		Phase: tasklifecycle.DispatchResources, Record: tasklifecycle.RecordAbsent,
		Resources: &assessment, Failure: result.err, StoreCloseError: closeFault,
	})
	response := jsonResponseFor(t, "dispatch", joined)
	if joined.code != 1 || !errors.Is(joined.err, closeFault) || response.Failure == nil ||
		response.Failure.Code == "resource_pressure" || response.Failure.NextAction != "stop_and_report" || response.Failure.RetryAfterSeconds != 0 {
		t.Fatalf("joined cleanup fault inherited CPU retry guidance: code=%d failure=%+v", joined.code, response.Failure)
	}
	if response.ResourceAdmission == nil || response.ResourceAdmission.Status != ResourceAdmissionPressure || response.ResourceAdmission.Diagnostics != nil {
		t.Fatal("an ordinary joined failure must preserve existing metadata without new refusal diagnostics")
	}
	assertNoManagedWorkspace(t, fixture.root, deps)
}

func TestResourceCPUDiagnosticsFollowContinueRefusal(t *testing.T) {
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{cpuPressureNow}}
	fixture := newResourcePiFixture(t, true, resources)
	result := dispatchInteractivePiResume(fixture)
	assertCPURefusalWithoutSuccessor(t, fixture, "continue", result)
	if checks, scopes := resources.calls(); checks != 1 || scopes != 0 {
		t.Fatalf("continue checks=%d scopes=%d want=1,0", checks, scopes)
	}
	if len(fixture.candidateSessionRoots) != 0 {
		t.Fatalf("refused continue prepared candidates: %v", fixture.candidateSessionRoots)
	}
}

func TestResourceCPUDiagnosticsFollowPrecreationRefresh(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		initial func() resourceadmission.Assessment
		scope   resourceadmission.Scope
	}{
		{name: "stale refresh", initial: clearStale},
		{name: "changed target refresh", initial: clearNow, scope: resourceadmission.Scope{TargetIdentity: "changed"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resources := &recordingResources{scope: testCase.scope, assessments: []func() resourceadmission.Assessment{testCase.initial, cpuPressureNow}}
			fixture := newResourcePiFixture(t, false, resources)
			result := dispatch(freshPiDispatchArguments(fixture), fixture.deps.normalized())
			assertCPURefusalWithoutSuccessor(t, fixture, "dispatch", result)
			if checks, scopes := resources.calls(); checks != 2 || scopes != 1 {
				t.Fatalf("refresh checks=%d scopes=%d want=2,1", checks, scopes)
			}
		})
	}
}

func assertCPURefusalWithoutSuccessor(t *testing.T, fixture *interactivePiResumeFixture, command string, result commandResult) {
	t.Helper()
	if result.code != 1 || !errors.Is(result.err, tasklifecycle.ErrResourcePressure) {
		t.Fatalf("CPU pressure did not refuse: %+v", result)
	}
	assertCPUResourceRefusal(t, jsonResponseFor(t, command, result))
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
	if entries := taskEntries(t, fixture.root); len(entries) != 1 || entries[0] != fixture.predecessorID {
		t.Fatalf("CPU refusal changed predecessor or created successor: %v", entries)
	}
}
