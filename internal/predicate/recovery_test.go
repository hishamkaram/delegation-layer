package predicate

import (
	"errors"
	"io"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type recoveryTestInterpreter struct {
	ref    task.PredicateRef
	target task.PredicateRef
	accept func(task.PredicateRef) bool
}

func (i recoveryTestInterpreter) Reference() task.PredicateRef { return i.ref }
func (i recoveryTestInterpreter) Evaluate(Input, Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "test"}, nil
}
func (i recoveryTestInterpreter) Target() task.PredicateRef { return i.target }
func (i recoveryTestInterpreter) CanRecover(source task.PredicateRef) bool {
	return i.accept == nil || i.accept(source)
}

func (i recoveryTestInterpreter) Recover(Input, Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "test"}, nil
}

func recoveryTestRef(adapter, version string) task.PredicateRef {
	return task.PredicateRef{Adapter: adapter, Mode: "read-only", Version: version, SHA256: task.ComputeSHA256([]byte(adapter + version))}
}

func TestRegistryResolvesOneRecoveryTargetIndependentOfRegistrationOrder(t *testing.T) {
	source := task.FixturePredicateRef()
	target := recoveryTestRef("codex:exec", "current")
	candidate := recoveryTestInterpreter{ref: target, target: target, accept: func(ref task.PredicateRef) bool { return ref.Equal(source) }}
	first, err := New(candidate)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(candidate)
	if err != nil {
		t.Fatal(err)
	}
	for _, registry := range []Registry{first, second} {
		got, resolveErr := registry.ResolveRecovery(source)
		if resolveErr != nil || got == nil || !got.Target().Equal(target) {
			t.Fatalf("recovery target=%v err=%v", got, resolveErr)
		}
	}
}

func TestRegistryRefusesUnsupportedAmbiguousAndSelfRecovery(t *testing.T) {
	source := task.FixturePredicateRef()
	unsupported, err := New(recoveryTestInterpreter{ref: recoveryTestRef("codex:exec", "unsupported"), target: recoveryTestRef("codex:exec", "unsupported"), accept: func(task.PredicateRef) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unsupported.ResolveRecovery(source); !errors.Is(err, ErrRecoveryUnsupported) {
		t.Fatalf("unsupported recovery error=%v", err)
	}

	firstRef := recoveryTestRef("codex:exec", "one")
	secondRef := recoveryTestRef("claude:print", "two")
	ambiguous, err := New(
		recoveryTestInterpreter{ref: firstRef, target: firstRef},
		recoveryTestInterpreter{ref: secondRef, target: secondRef},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ambiguous.ResolveRecovery(source); !errors.Is(err, ErrRecoveryAmbiguous) {
		t.Fatalf("ambiguous recovery error=%v", err)
	}

	selfRef := source
	self, err := New(recoveryTestInterpreter{ref: selfRef, target: source, accept: func(ref task.PredicateRef) bool { return ref.Equal(source) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = self.ResolveRecovery(source); !errors.Is(err, ErrRecoveryUnsupported) {
		t.Fatalf("self/source recovery error=%v", err)
	}
}

func TestRegistryRejectsUnregisteredRecoveryTarget(t *testing.T) {
	ref := recoveryTestRef("codex:exec", "registered")
	target := recoveryTestRef("codex:exec", "unregistered")
	if _, err := New(recoveryTestInterpreter{ref: ref, target: target}); err == nil {
		t.Fatal("unregistered recovery target accepted")
	}
}
