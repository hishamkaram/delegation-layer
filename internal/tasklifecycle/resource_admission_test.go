package tasklifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// scriptedResources returns its assessments in order and counts every call.
// A nil script makes any use a test failure.
type scriptedResources struct {
	t           *testing.T
	assessments []resourceadmission.Assessment
	scope       resourceadmission.Scope
	checks      int
	scopes      int
	onCheck     func()
}

func (s *scriptedResources) Check(context.Context, resourceadmission.TargetResolver) (resourceadmission.Assessment, error) {
	if s.checks >= len(s.assessments) {
		s.t.Fatalf("unexpected resource check %d", s.checks+1)
	}
	s.checks++
	if s.onCheck != nil {
		s.onCheck()
	}
	return s.assessments[s.checks-1], nil
}

func (s *scriptedResources) Scope(context.Context, resourceadmission.TargetResolver) (resourceadmission.Scope, error) {
	s.scopes++
	return s.scope, nil
}

func pressureAssessment(at time.Time) resourceadmission.Assessment {
	return resourceadmission.NewAssessment(at, resourceadmission.Scope{}, resourceadmission.Findings{
		Pressures:   []resourceadmission.Pressure{{Metric: resourceadmission.MetricHostMemory, Basis: resourceadmission.BasisCritical}},
		Unavailable: []resourceadmission.Unavailable{{Metric: resourceadmission.MetricHostCPU, Reason: resourceadmission.ReasonDeadline}},
	})
}

func clearAssessment(at time.Time) resourceadmission.Assessment {
	return resourceadmission.NewAssessment(at, resourceadmission.Scope{}, resourceadmission.Findings{})
}

func newResourceGateStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func TestResourcePressureRefusalClassifiesWholeError(t *testing.T) {
	refusal := resourceRefusal(pressureAssessment(time.Now()))
	staged := admissioncore.WithStage("check-resources", refusal)
	if pressure, ok := ResourcePressureRefusal(staged); !ok || pressure.Metric != resourceadmission.MetricHostMemory || pressure.Basis != resourceadmission.BasisCritical {
		t.Fatalf("wrapped refusal was not classified: %+v %t", pressure, ok)
	}
	closeFault := errors.New("close store")
	for name, err := range map[string]error{
		"joined close fault":    errors.Join(staged, closeFault),
		"marker only":           admissioncore.WithStage("check-resources", ErrResourcePressure),
		"cancellation at stage": admissioncore.WithStage("check-resources", context.Canceled),
		"no error":              nil,
	} {
		if _, ok := ResourcePressureRefusal(err); ok {
			t.Errorf("%s was classified as a pure resource refusal: %v", name, err)
		}
		if name == "joined close fault" && (!errors.Is(err, ErrResourcePressure) || !errors.Is(err, closeFault)) {
			t.Errorf("joined error lost an identity: %v", err)
		}
	}
	if resourceRefusal(clearAssessment(time.Now())) != nil || resourceRefusal(resourceadmission.Assessment{}) != nil {
		t.Fatal("a clear or unobserved assessment refused admission")
	}
}

// An existing, partial, or symlinked task entry belongs to existing-task
// recovery: the gate must not sample or refuse it.
func TestResourceGateLeavesPresentTaskEntriesUngated(t *testing.T) {
	const id = "cccccccccccccccccccccccccccccccc"
	for name, create := range map[string]func(string) error{
		"partial directory": func(path string) error { return os.MkdirAll(path, 0o700) },
		"regular file":      func(path string) error { return os.WriteFile(path, nil, 0o600) },
		"dangling symlink":  func(path string) error { return os.Symlink(filepath.Join(filepath.Dir(path), "missing"), path) },
	} {
		t.Run(name, func(t *testing.T) {
			store := newResourceGateStore(t)
			if err := os.MkdirAll(filepath.Join(store.Root, "tasks"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := create(filepath.Join(store.Root, "tasks", id)); err != nil {
				t.Fatal(err)
			}
			checker := &scriptedResources{t: t}
			gate, err := openResourceGate(DispatchRequest{}, DispatchOptions{Resources: checker}, store, id)
			if gate != nil || err != nil || checker.checks != 0 || checker.scopes != 0 {
				t.Fatalf("present entry was gated: gate=%v err=%v checks=%d", gate, err, checker.checks)
			}
		})
	}
}

func TestResourceGateRefusesOnlyWhileTaskEntryIsAbsent(t *testing.T) {
	const id = "dddddddddddddddddddddddddddddddd"
	store := newResourceGateStore(t)
	checker := &scriptedResources{t: t, assessments: []resourceadmission.Assessment{pressureAssessment(time.Now())}}
	gate, err := openResourceGate(DispatchRequest{}, DispatchOptions{Resources: checker}, store, id)
	if _, ok := ResourcePressureRefusal(err); !ok || gate.current() == nil || gate.current().Status() != resourceadmission.StatusPressure {
		t.Fatalf("absent entry under pressure was not refused: gate=%v err=%v", gate, err)
	}
	if err = os.MkdirAll(filepath.Join(store.Root, "tasks", id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = gate.refusal(); err != nil {
		t.Fatalf("an entry that appeared before refusal was refused: %v", err)
	}
}

func TestResourceGateCPUObservationDoesNotRefuseAnEntryThatAppears(t *testing.T) {
	const id = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	store := newResourceGateStore(t)
	busy, total, fraction := 95.0, 100.0, 0.95
	assessment := resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, resourceadmission.Findings{
		Pressures: []resourceadmission.Pressure{{Metric: resourceadmission.MetricHostCPU, Basis: resourceadmission.BasisSustained}},
		HostCPU: &resourceadmission.HostCPUDiagnostics{
			CounterUnit: resourceadmission.CPUCounterTicks, BusyThreshold: 0.95, RequiredStreak: 3,
			Intervals: []resourceadmission.HostCPUInterval{{
				ElapsedMilliseconds: 1000, BusyDelta: &busy, TotalDelta: &total, BusyFraction: &fraction,
				Classification: resourceadmission.CPUIntervalHot,
			}},
		},
	})
	checker := &scriptedResources{t: t, assessments: []resourceadmission.Assessment{assessment}, onCheck: func() {
		if err := os.MkdirAll(filepath.Join(store.Root, "tasks", id), 0o700); err != nil {
			t.Fatal(err)
		}
	}}
	gate, err := openResourceGate(DispatchRequest{}, DispatchOptions{Resources: checker}, store, id)
	if err != nil || checker.checks != 1 || gate.current() == nil || gate.current().HostCPUDiagnostics() == nil {
		t.Fatalf("appearing entry must keep existing-task flow and its observation: err=%v checks=%d gate=%+v", err, checker.checks, gate)
	}
}

// A task entry that appears after the initial check, while preparation runs,
// belongs to existing-task recovery: refresh must not read any metric for it.
func TestResourceGateRefreshLeavesEntryThatAppearedUngated(t *testing.T) {
	const id = "abababababababababababababababab"
	store := newResourceGateStore(t)
	checker := &scriptedResources{t: t, assessments: []resourceadmission.Assessment{clearAssessment(time.Now().Add(-10 * time.Second))}}
	gate, err := openResourceGate(DispatchRequest{}, DispatchOptions{Resources: checker}, store, id)
	if err != nil {
		t.Fatal(err)
	}
	accepted := filepath.Join(store.Root, "tasks", id, "accepted")
	if err = os.MkdirAll(filepath.Dir(accepted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(accepted, []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = gate.refresh(task.SupervisorRef{}, supervisorcore.AdmissionOptions{}); err != nil {
		t.Fatalf("refresh of an entry that appeared = %v", err)
	}
	if checker.checks != 1 || checker.scopes != 0 {
		t.Fatalf("refresh read resources for a present entry: checks=%d scopes=%d", checker.checks, checker.scopes)
	}
	if data, readErr := os.ReadFile(accepted); readErr != nil || string(data) != "work" {
		t.Fatalf("present entry changed: %q %v", data, readErr)
	}
}

func TestResourceGateRefreshUsesFreshnessAndScope(t *testing.T) {
	const id = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	now := time.Now()
	for _, testCase := range []struct {
		name       string
		initial    resourceadmission.Assessment
		scope      resourceadmission.Scope
		wantChecks int
	}{
		{name: "fresh same scope reused", initial: clearAssessment(now), wantChecks: 1},
		{name: "stale assessment rechecked", initial: clearAssessment(now.Add(-10 * time.Second)), wantChecks: 2},
		{name: "changed target rechecked", initial: clearAssessment(now), scope: resourceadmission.Scope{TargetIdentity: "supervisor:7:t"}, wantChecks: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := newResourceGateStore(t)
			checker := &scriptedResources{t: t, scope: testCase.scope, assessments: []resourceadmission.Assessment{testCase.initial, pressureAssessment(time.Now())}}
			gate, err := openResourceGate(DispatchRequest{}, DispatchOptions{Resources: checker}, store, id)
			if err != nil {
				t.Fatal(err)
			}
			err = gate.refresh(task.SupervisorRef{}, supervisorcore.AdmissionOptions{})
			if checker.checks != testCase.wantChecks || checker.scopes != 1 {
				t.Fatalf("refresh checks=%d scopes=%d, want %d checks and one scope", checker.checks, checker.scopes, testCase.wantChecks)
			}
			_, refused := ResourcePressureRefusal(err)
			if refused != (testCase.wantChecks == 2) || (testCase.wantChecks == 1 && err != nil) {
				t.Fatalf("refresh refusal=%t err=%v", refused, err)
			}
			if testCase.wantChecks == 2 && gate.current().Status() != resourceadmission.StatusPressure {
				t.Fatalf("refresh did not keep its latest assessment: %+v", gate.current())
			}
		})
	}
}
