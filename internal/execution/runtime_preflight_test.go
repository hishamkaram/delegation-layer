package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func configurePendingRuntimePreflight(meta *task.MetaRecord, plan *Plan) {
	meta.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("required-runtime-capability"))
	meta.ProviderVersion = ""
	meta.EnvironmentRecorded = true
	meta.Environment = []string{"DELEGATE_EXECUTION_HELPER=echo"}
	plan.Environment = append([]string{}, meta.Environment...)
	plan.RuntimePreflightSHA256 = meta.RuntimePreflightSHA256
}

func finalizedRuntimePlan(plan Plan) Plan {
	plan.RuntimePreflightSHA256 = ""
	plan.ProviderRuntime = task.ProviderRuntimeIdentity{
		Executable: plan.Executable,
		Version:    "fixture-observed-v2",
		SHA256:     plan.ExecutableSHA256,
	}
	return plan
}

func assertPendingRuntimeRejected(t *testing.T, td *taskdir.TaskDir, result Result) {
	t.Helper()
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("pending runtime did not publish a rejection: %+v", result)
	}
	inspection, err := td.Inspect()
	require(t, err)
	if !inspection.StartExists || !inspection.SealExists || !inspection.OutcomeExists || inspection.StartedExists {
		t.Fatalf("pending runtime refusal left incorrect authority: %+v", inspection)
	}
	sealData, err := os.ReadFile(filepath.Join(td.Dir, "provider.exit"))
	require(t, err)
	var seal task.ProviderExitRecord
	require(t, task.DecodeStrict(sealData, &seal))
	identity := task.ProviderRuntimeIdentity{Executable: seal.ProviderExecutable, Version: seal.ProviderVersion, SHA256: seal.ProviderSHA256}
	if seal.InvocationState != task.InvocationStartFailed || identity != (task.ProviderRuntimeIdentity{}) {
		t.Fatalf("pending runtime refusal fabricated a provider launch or identity: %+v", seal)
	}
	if _, err = td.PrepareStart(0); !errors.Is(err, task.ErrAlreadyStarted) {
		t.Fatalf("pending runtime refusal regained start authority: %v", err)
	}
	replayed, cleanupErr, collectErr := td.Collect(seal.Predicate)
	require(t, errors.Join(cleanupErr, collectErr))
	if replayed == nil || replayed.Verdict != result.Outcome.Verdict || replayed.EvidenceSHA256 != result.Outcome.EvidenceSHA256 {
		t.Fatalf("pending runtime refusal changed during collection: %+v", replayed)
	}
	sealAfter, err := os.ReadFile(filepath.Join(td.Dir, "provider.exit"))
	require(t, err)
	if !bytes.Equal(sealData, sealAfter) {
		t.Fatal("collection replaced the failed preflight seal")
	}
}

func TestPendingRuntimeRequiresPlanCallback(t *testing.T) {
	for _, errorOnly := range []bool{false, true} {
		name := "no-callback"
		if errorOnly {
			name = "error-only-preflight"
		}
		t.Run(name, func(t *testing.T) {
			td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configurePendingRuntimePreflight)
			startCalls, preflightCalls := 0, 0
			opts := Options{Hooks: Hooks{Start: func(*exec.Cmd) error {
				startCalls++
				return errors.New("unexpected provider launch")
			}}}
			if errorOnly {
				opts.Preflight = func(PreflightScope) error {
					preflightCalls++
					return nil
				}
			}
			result := Run(td, permit, plan, opts)
			assertPendingRuntimeRejected(t, td, result)
			if startCalls != 0 || preflightCalls != 0 {
				t.Fatalf("missing plan callback ran preflight=%d provider=%d", preflightCalls, startCalls)
			}
			if !strings.Contains(string(payload(t, td, result.Outcome)), "runtime preflight has no plan callback") {
				t.Fatal("rejection did not explain the missing runtime preflight callback")
			}
		})
	}
}

func TestPendingRuntimeRejectsUnfinishedPreflight(t *testing.T) {
	cases := []struct {
		name     string
		finalize func(Plan) (Plan, error)
	}{
		{"failed", func(plan Plan) (Plan, error) {
			return finalizedRuntimePlan(plan), errors.New("required runtime capability unavailable")
		}},
		{"no-result", func(Plan) (Plan, error) { return Plan{}, nil }},
		{"still-pending", func(plan Plan) (Plan, error) { return plan, nil }},
		{"different-pending-contract", func(plan Plan) (Plan, error) {
			plan.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("different-contract"))
			return plan, nil
		}},
		{"no-observation", func(plan Plan) (Plan, error) {
			plan.RuntimePreflightSHA256 = ""
			return plan, nil
		}},
		{"missing-version", func(plan Plan) (Plan, error) {
			plan = finalizedRuntimePlan(plan)
			plan.ProviderRuntime.Version = ""
			return plan, nil
		}},
		{"wrong-executable", func(plan Plan) (Plan, error) {
			plan = finalizedRuntimePlan(plan)
			plan.ProviderRuntime.Executable = filepath.Join(plan.Directory, "another-provider")
			return plan, nil
		}},
		{"wrong-digest", func(plan Plan) (Plan, error) {
			plan = finalizedRuntimePlan(plan)
			plan.ProviderRuntime.SHA256 = task.ComputeSHA256([]byte("another-provider"))
			return plan, nil
		}},
		{"unrecordable-version", func(plan Plan) (Plan, error) {
			plan = finalizedRuntimePlan(plan)
			plan.ProviderRuntime.Version = strings.Repeat("v", task.MaxProviderRuntimeEvidenceVersionBytes+1)
			return plan, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configurePendingRuntimePreflight)
			startCalls, preflightCalls := 0, 0
			events := &eventLog{}
			result := Run(td, permit, plan, Options{
				PreflightPlan: func(_ PreflightScope, admitted Plan) (Plan, error) {
					preflightCalls++
					return tc.finalize(admitted)
				},
				Hooks: Hooks{Event: events.add, Start: func(*exec.Cmd) error {
					startCalls++
					return errors.New("unexpected provider launch")
				}},
			})
			assertPendingRuntimeRejected(t, td, result)
			if startCalls != 0 || preflightCalls != 1 {
				t.Fatalf("unfinished preflight: preflight=%d start=%d", preflightCalls, startCalls)
			}
			counts := make(map[string]int)
			for _, event := range events.snapshot() {
				counts[event]++
			}
			if counts["sealed"] != 1 || counts["published"] != 1 {
				t.Fatalf("refusal was not sealed and published exactly once: %v", counts)
			}
		})
	}
}

func TestPendingRuntimeBindsImmutableAdmissionMarker(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Plan)
	}{
		{"dropped", func(plan *Plan) { plan.RuntimePreflightSHA256 = "" }},
		{"changed", func(plan *Plan) { plan.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("different-contract")) }},
		{"invalid", func(plan *Plan) { plan.RuntimePreflightSHA256 = "invalid" }},
		{"already-finalized", func(plan *Plan) { *plan = finalizedRuntimePlan(*plan) }},
		{"premature-observation", func(plan *Plan) { plan.ProviderRuntime = finalizedRuntimePlan(*plan).ProviderRuntime }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configurePendingRuntimePreflight)
			tc.change(&plan)
			preflightCalls, startCalls := 0, 0
			result := Run(td, permit, plan, Options{
				PreflightPlan: func(_ PreflightScope, admitted Plan) (Plan, error) {
					preflightCalls++
					return finalizedRuntimePlan(admitted), nil
				},
				Hooks: Hooks{Start: func(*exec.Cmd) error {
					startCalls++
					return errors.New("unexpected provider launch")
				}},
			})
			if !errors.Is(result.Error, task.ErrIdentityMismatch) || permit.IsConsumed() || preflightCalls != 0 || startCalls != 0 {
				t.Fatalf("changed admission marker gained execution authority: %+v preflight=%d start=%d", result, preflightCalls, startCalls)
			}
		})
	}
}

func TestPendingRuntimePreflightLaunchesOnceWithObservedIdentity(t *testing.T) {
	td, permit, plan, brief := fixtureTaskWithPlan(t, "echo", configurePendingRuntimePreflight)
	metaBefore, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
	require(t, err)
	preflightCalls, startCalls, waitCalls := 0, 0, 0
	result := Run(td, permit, plan, Options{
		PreflightPlan: func(_ PreflightScope, admitted Plan) (Plan, error) {
			preflightCalls++
			if admitted.RuntimePreflightSHA256 != plan.RuntimePreflightSHA256 || admitted.ProviderRuntime != (task.ProviderRuntimeIdentity{}) {
				return Plan{}, errors.New("preflight did not receive the pending admitted contract")
			}
			return finalizedRuntimePlan(admitted), nil
		},
		Hooks: Hooks{
			Start: func(cmd *exec.Cmd) error {
				startCalls++
				return cmd.Start()
			},
			Wait: func(cmd *exec.Cmd) error {
				waitCalls++
				return cmd.Wait()
			},
		},
	})
	assertSuccess(t, result)
	if preflightCalls != 1 || startCalls != 1 || waitCalls != 1 || !bytes.Equal(payload(t, td, result.Outcome), brief) {
		t.Fatalf("pending runtime did not launch exactly once: preflight=%d start=%d wait=%d", preflightCalls, startCalls, waitCalls)
	}
	assertObservedRuntimeEvidence(t, td, finalizedRuntimePlan(plan).ProviderRuntime)
	metaAfter, err := os.ReadFile(filepath.Join(td.Dir, "meta.json"))
	require(t, err)
	var meta task.MetaRecord
	require(t, task.DecodeStrict(metaAfter, &meta))
	if !bytes.Equal(metaBefore, metaAfter) || meta.RuntimePreflightSHA256 != plan.RuntimePreflightSHA256 || meta.ProviderVersion != "" {
		t.Fatal("successful runtime preflight rewrote immutable admission metadata")
	}
	if _, err = td.PrepareStart(0); !errors.Is(err, task.ErrAlreadyStarted) {
		t.Fatalf("successful runtime preflight regained start authority: %v", err)
	}
}

func assertObservedRuntimeEvidence(t *testing.T, td *taskdir.TaskDir, expected task.ProviderRuntimeIdentity) {
	t.Helper()
	startedData, err := os.ReadFile(filepath.Join(td.Dir, "provider.started.json"))
	require(t, err)
	var started task.ProviderStartedRecord
	require(t, task.DecodeStrict(startedData, &started))
	if started.ProviderExecutable != expected.Executable || started.ProviderVersion != expected.Version || started.ProviderSHA256 != expected.SHA256 {
		t.Fatalf("start receipt lost the observed provider identity: %+v", started)
	}
	sealData, err := os.ReadFile(filepath.Join(td.Dir, "provider.exit"))
	require(t, err)
	var seal task.ProviderExitRecord
	require(t, task.DecodeStrict(sealData, &seal))
	if seal.InvocationState != task.InvocationStarted || seal.ProviderExecutable != expected.Executable || seal.ProviderVersion != expected.Version || seal.ProviderSHA256 != expected.SHA256 {
		t.Fatalf("seal lost the observed provider identity: %+v", seal)
	}
}

func TestPendingRuntimeRefreshPreservesAdmittedLaunchContract(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Plan)
	}{
		{"directory", func(plan *Plan) { plan.Directory = filepath.Dir(plan.Directory) }},
		{"predicate", func(plan *Plan) { plan.Predicate.Adapter = "fixture:changed" }},
		{"arguments", func(plan *Plan) { plan.Arguments = append([]string{"changed"}, plan.Arguments[1:]...) }},
		{"input-files", func(plan *Plan) { plan.InputFiles = nil }},
		{"output-artifacts", func(plan *Plan) { plan.OutputArtifacts = nil }},
		{"output-contract", func(plan *Plan) { plan.OutputWriterContract = "changed" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", func(meta *task.MetaRecord, plan *Plan) {
				configurePendingRuntimePreflight(meta, plan)
				configureLaunchFiles(meta, plan)
			})
			startCalls := 0
			result := Run(td, permit, plan, Options{
				PreflightPlan: func(_ PreflightScope, admitted Plan) (Plan, error) {
					refreshed := finalizedRuntimePlan(admitted)
					tc.change(&refreshed)
					return refreshed, nil
				},
				Hooks: Hooks{Start: func(*exec.Cmd) error {
					startCalls++
					return errors.New("unexpected provider launch")
				}},
			})
			assertPendingRuntimeRejected(t, td, result)
			if startCalls != 0 {
				t.Fatal("refreshed runtime changed the admitted launch contract")
			}
		})
	}
}

func TestPendingRuntimePreflightCannotRenewDeadline(t *testing.T) {
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configurePendingRuntimePreflight)
	clock := newAdvancingClock()
	deadline := clock.Now().Add(30 * time.Second)
	startCalls := 0
	result := Run(td, permit, plan, Options{
		Clock: clock, Deadline: deadline,
		Stopper: stopperFunc(func(context.Context, time.Time) error { return nil }),
		PreflightPlan: func(scope PreflightScope, admitted Plan) (Plan, error) {
			if observed, ok := scope.Context().Deadline(); !ok || !observed.Equal(deadline) {
				return Plan{}, errors.New("preflight lost the original deadline")
			}
			clock.advance(time.Minute)
			return finalizedRuntimePlan(admitted), nil
		},
		Hooks: Hooks{Start: func(*exec.Cmd) error {
			startCalls++
			return errors.New("unexpected provider launch")
		}},
	})
	assertPendingRuntimeRejected(t, td, result)
	if startCalls != 0 || !strings.Contains(string(payload(t, td, result.Outcome)), errBudgetExpiredBeforeStart.Error()) {
		t.Fatalf("late runtime preflight did not preserve the deadline: starts=%d result=%+v", startCalls, result)
	}
}
