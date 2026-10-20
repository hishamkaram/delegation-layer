package codex

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var interactionModes = []string{Mode, WorkspaceWriteMode}

// Frozen digests of the shipped interactive predicates. They pin the exact
// contract bytes so a later edit to a historical contract cannot go unnoticed.
var frozenInteractionDigests = map[string]map[string]string{
	"interactive-v1": {
		Mode:               "8df1a1a8899214f44ca2aa2ecaed4fd186c3734de7da4934f1e2b28c090eab1b",
		WorkspaceWriteMode: "884ca92f866255625e9e689c51bad2d6f23ea3fa47c919566f547b2b15eeb5b8",
	},
	"interactive-v2": {
		Mode:               "3c35dd1876911b593a7cf677f59bad9eebfc9f17372503d1f3f6fe815f8a7e42",
		WorkspaceWriteMode: "48307e62a0b5e26cb916b263100f035db4a79f4ea2c814f7bd2493a59a5dfda2",
	},
}

func TestShippedInteractivePredicatesAreFrozen(t *testing.T) {
	for _, mode := range interactionModes {
		for version, reference := range map[string]task.PredicateRef{
			"interactive-v1": historicalInteractionReference(mode),
			"interactive-v2": strictInteractionReference(mode),
		} {
			if reference.Version != version || reference.SHA256 != frozenInteractionDigests[version][mode] {
				t.Fatalf("%s %s reference changed: %+v", version, mode, reference)
			}
		}
	}
}

func TestSuccessorInteractivePredicateIsDistinctAndBindsItsContract(t *testing.T) {
	if !strings.HasSuffix(currentInteractionContract, "\n") {
		t.Fatal("current contract is not canonical")
	}
	for _, mode := range interactionModes {
		current := interactionReference(mode)
		if current.Version != "interactive-v3" || current.Adapter != Provider || current.Mode != mode {
			t.Fatalf("current reference=%+v", current)
		}
		if current.SHA256 != task.ComputeSHA256([]byte(currentInteractionContract+mode+"\n")) {
			t.Fatalf("current digest does not cover canonical contract bytes: %+v", current)
		}
		for _, frozen := range []task.PredicateRef{historicalInteractionReference(mode), strictInteractionReference(mode)} {
			if current.Equal(frozen) || current.SHA256 == frozen.SHA256 {
				t.Fatalf("current %+v collides with frozen %+v", current, frozen)
			}
		}
		if err := task.ValidatePredicateRef(current); err != nil {
			t.Fatal(err)
		}
	}
	if interactionReference(Mode).SHA256 == interactionReference(WorkspaceWriteMode).SHA256 {
		t.Fatal("current predicate digest does not bind the mode")
	}
}

func TestRegistrationResolvesEveryInteractiveRevisionAndOneRecoveryPath(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range interactionModes {
		for _, revision := range interactionRevisions {
			resolved, resolveErr := registry.Resolve(revision.reference(mode))
			if resolveErr != nil || !resolved.Reference().Equal(revision.reference(mode)) {
				t.Fatalf("%s %d not registered: %v", mode, revision, resolveErr)
			}
		}
		for source, target := range map[task.PredicateRef]task.PredicateRef{
			historicalInteractionReference(mode): strictInteractionReference(mode),
			strictInteractionReference(mode):     interactionReference(mode),
		} {
			recovery, recoverErr := registry.ResolveRecovery(source)
			if recoverErr != nil || !recovery.Target().Equal(target) {
				t.Fatalf("%s recovery of %+v = %v, %v", mode, source, recovery, recoverErr)
			}
		}
		if _, recoverErr := registry.ResolveRecovery(interactionReference(mode)); !errors.Is(recoverErr, predicate.ErrRecoveryUnsupported) {
			t.Fatalf("current predicate has a recovery source: %v", recoverErr)
		}
	}
}

func TestRecoveryAssessesSealedStrictTraceWithoutChangingTheSeal(t *testing.T) {
	trace := joinTrace(nativeSetupTrace(), standardTurnTrace())
	stdout := []byte(strings.Join(trace, "\n") + "\n")
	seal := validSeal(stdout, nil)
	seal.Predicate = strictInteractionReference(Mode)
	sealed := seal

	strict, _ := traceRun{revision: interactionStrict, lines: trace}.evaluate(t)
	if strict.Verdict != task.VerdictRejected {
		t.Fatalf("frozen strict replay changed: %+v", strict)
	}
	recoverer, ok := newInteractionInterpreter(Mode).(predicate.RecoveryInterpreter)
	if !ok || !recoverer.CanRecover(seal.Predicate) || !recoverer.Target().Equal(interactionReference(Mode)) {
		t.Fatal("current interpreter does not declare interactive-v2 recovery")
	}
	var answer bytes.Buffer
	result, err := recoverer.Recover(predicate.Input{Seal: seal}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || result.Verdict != task.VerdictCommitted || answer.String() != traceAnswer {
		t.Fatalf("recovered result=%+v answer=%q err=%v", result, answer.String(), err)
	}
	if !reflect.DeepEqual(seal, sealed) || !seal.Predicate.Equal(strictInteractionReference(Mode)) {
		t.Fatalf("recovery mutated the original seal: %+v", seal)
	}
}

func TestRecoveryNeverCommitsTheIncompleteOriginalTrace(t *testing.T) {
	// The Linux Codex 0.163.0 failure: input closed at thread/started, so
	// the turn never started and the supervisor sealed a protocol fault.
	incomplete := nativeSetupTrace()[:7]
	stdout := []byte(strings.Join(incomplete, "\n") + "\n")
	recoverer, ok := newInteractionInterpreter(Mode).(predicate.RecoveryInterpreter)
	if !ok {
		t.Fatal("current interpreter is not a recoverer")
	}
	for name, alter := range map[string]func(*task.ProviderExitRecord){
		"sealed protocol fault": func(s *task.ProviderExitRecord) { s.Error = "interactive protocol fault" },
		"clean seal":            func(*task.ProviderExitRecord) {},
	} {
		seal := alteredSeal(stdout, nil, func(s *task.ProviderExitRecord) {
			s.Predicate = strictInteractionReference(Mode)
			alter(s)
		})
		var answer bytes.Buffer
		result, err := recoverer.Recover(predicate.Input{Seal: seal}, &testEvidence{stdout: stdout}, &answer)
		if err != nil || result.Verdict != task.VerdictRejected || answer.Len() != 0 {
			t.Fatalf("%s result=%+v answer=%q err=%v", name, result, answer.String(), err)
		}
	}
	complete := []byte(strings.Join(joinTrace(nativeSetupTrace(), standardTurnTrace()), "\n") + "\n")
	faulted := alteredSeal(complete, nil, func(s *task.ProviderExitRecord) {
		s.Predicate = strictInteractionReference(Mode)
		s.Error = "interactive protocol fault"
	})
	result, err := recoverer.Recover(predicate.Input{Seal: faulted}, &testEvidence{stdout: complete}, &bytes.Buffer{})
	if err != nil || result.Verdict != task.VerdictRejected {
		t.Fatalf("a nonempty sealed error recovered: %+v err=%v", result, err)
	}
}

func TestRecoveryRefusesSourcesOutsideItsDeclaration(t *testing.T) {
	current, strict := newInteractionInterpreter(Mode), newStrictInteractionInterpreter(Mode)
	for _, test := range []struct {
		name        string
		interpreter predicate.Interpreter
		source      task.PredicateRef
		want        bool
	}{
		{"current from v2", current, strictInteractionReference(Mode), true},
		{"current from v1", current, historicalInteractionReference(Mode), false},
		{"current from itself", current, interactionReference(Mode), false},
		{"current from other mode", current, strictInteractionReference(WorkspaceWriteMode), false},
		{"v2 from v1", strict, historicalInteractionReference(Mode), true},
		{"v2 from itself", strict, strictInteractionReference(Mode), false},
		{"v2 from current", strict, interactionReference(Mode), false},
		{"v1 from anything", newHistoricalInteractionInterpreter(Mode), historicalInteractionReference(Mode), false},
	} {
		recoverer, ok := test.interpreter.(predicate.RecoveryInterpreter)
		if !ok || recoverer.CanRecover(test.source) != test.want {
			t.Fatalf("%s: CanRecover=%v want=%v", test.name, ok && recoverer.CanRecover(test.source), test.want)
		}
	}
	recoverer, ok := current.(predicate.RecoveryInterpreter)
	if !ok {
		t.Fatal("current interpreter is not a recoverer")
	}
	seal := validSeal(nil, nil)
	seal.Predicate = historicalInteractionReference(Mode)
	if _, err := recoverer.Recover(predicate.Input{Seal: seal}, &testEvidence{}, &bytes.Buffer{}); !errors.Is(err, task.ErrIncompatiblePredicate) {
		t.Fatalf("recovery from v1 by the current interpreter: %v", err)
	}
}
