package supervisor

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestValidateContinuationSelectionRunnerComparison(t *testing.T) {
	root := t.TempDir()
	recorded := filepath.Join(root, "recorded-runner")
	other := filepath.Join(root, "other-runner")
	external := filepath.Join(root, "external", "pueue.yml")
	meta := func(runner string) *task.MetaRecord {
		return &task.MetaRecord{RunnerExecutable: runner, SupervisorConfig: task.SupervisorRef{ConfigPath: external}}
	}
	for _, testCase := range []struct {
		name      string
		selection AdmissionSelection
		meta      *task.MetaRecord
		runner    bool
		identity  bool
		conflict  bool
		anyError  bool
	}{
		{name: "equal override", selection: AdmissionSelection{Runner: recorded}, meta: meta(recorded)},
		{name: "omitted override", meta: meta(recorded)},
		{name: "unequal override", selection: AdmissionSelection{Runner: other}, meta: meta(recorded), runner: true, identity: true, anyError: true},
		{name: "unrecorded runner with override", selection: AdmissionSelection{Runner: other}, meta: meta("")},
		{name: "neither runner recorded", meta: meta(""), anyError: true},
		{name: "config conflict beats equal runner", selection: AdmissionSelection{Runner: recorded, ConfigPath: filepath.Join(root, "other.yml")}, meta: meta(recorded), conflict: true, anyError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateContinuationSelection(root, testCase.selection, testCase.meta)
			if (err != nil) != testCase.anyError {
				t.Fatalf("error = %v, want error=%v", err, testCase.anyError)
			}
			if err == nil {
				return
			}
			if got := task.AllErrorCauses(fmt.Errorf("stage: %w", err), IsContinuationRunnerRequest); got != testCase.runner {
				t.Fatalf("runner refusal = %v, want %v: %v", got, testCase.runner, err)
			}
			if got := errors.Is(err, task.ErrIdentityMismatch); got != testCase.identity {
				t.Fatalf("identity mismatch = %v, want %v: %v", got, testCase.identity, err)
			}
			if got := errors.Is(err, task.ErrRequestConflict); got != testCase.conflict {
				t.Fatalf("request conflict = %v, want %v: %v", got, testCase.conflict, err)
			}
		})
	}
}

func TestContinuationRunnerRefusalNeverWidens(t *testing.T) {
	refusal := ValidateContinuationSelection(t.TempDir(), AdmissionSelection{Runner: "/other"}, &task.MetaRecord{RunnerExecutable: "/recorded"})
	if errors.Unwrap(refusal) != nil {
		t.Fatal("refusal exposes a cause that would widen its identity")
	}
	if task.AllErrorCauses(task.ErrIdentityMismatch, IsContinuationRunnerRequest) || task.AllErrorCauses(fmt.Errorf("%w: saved identity differs", task.ErrIdentityMismatch), IsContinuationRunnerRequest) {
		t.Fatal("a saved-identity fault was recognized as the runner refusal")
	}
	if task.AllErrorCauses(errors.Join(refusal, errors.New("storage fault")), IsContinuationRunnerRequest) {
		t.Fatal("a joined fault was recognized as the runner refusal")
	}
}

// wrappedRefusalError is a single-cause wrapper, the shape stage and context wrapping take.
type wrappedRefusalError struct{ cause error }

func (e wrappedRefusalError) Error() string { return "wrapped: " + e.cause.Error() }

func (e wrappedRefusalError) Unwrap() error { return e.cause }

// claimsEveryTargetError is an unrelated error whose Is method matches anything.
type claimsEveryTargetError struct{}

func (claimsEveryTargetError) Error() string { return "unrelated" }

func (claimsEveryTargetError) Is(error) bool { return true }

// TestContinuationRunnerPredicateIsExactLeafIdentity proves the leaf predicate
// accepts only the owned value itself. Ordinary wrapping is the traversal's
// job, and an unrelated Is method never owns the leaf.
func TestContinuationRunnerPredicateIsExactLeafIdentity(t *testing.T) {
	refusal := ValidateContinuationSelection(t.TempDir(), AdmissionSelection{Runner: "/other"}, &task.MetaRecord{RunnerExecutable: "/recorded"})
	if !IsContinuationRunnerRequest(refusal) {
		t.Fatal("the owned refusal is not its own exact leaf")
	}
	wrapped := wrappedRefusalError{refusal}
	if IsContinuationRunnerRequest(wrapped) {
		t.Fatal("a wrapper was accepted as the exact leaf")
	}
	if !task.AllErrorCauses(wrapped, IsContinuationRunnerRequest) {
		t.Fatal("whole-error traversal lost an ordinarily wrapped refusal")
	}
	if IsContinuationRunnerRequest(claimsEveryTargetError{}) || task.AllErrorCauses(claimsEveryTargetError{}, IsContinuationRunnerRequest) {
		t.Fatal("an unrelated Is method was accepted as the owned leaf")
	}
}
