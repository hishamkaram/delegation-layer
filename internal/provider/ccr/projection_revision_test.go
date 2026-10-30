package ccr

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The read-only route-preserving ccr-foreground-v7 reference recorded by
// completed tasks before the failed-request and quota clauses existed. It
// shares its version with the current reference and differs by digest.
const (
	ccrProjectionReadDigest = "c70936cf9a3b1a247ffd12403675dbc714bec8bb0a4179e85b6f348d918cf0cf"
	ccrCurrentReadDigest    = "7447125836ee96e2d8d16760d4dcb90c78d619bf75ef50f6343a012ffaf213cb"
)

func recordedCCRReference(mode, version, digest string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: digest}
}

// ccrProjectionNearMisses are references that differ from the recorded
// projection reference in one field and that no interpreter owns.
func ccrProjectionNearMisses() map[string]task.PredicateRef {
	altered := []byte(ccrProjectionReadDigest)
	altered[len(altered)-1] ^= 1
	return map[string]task.PredicateRef{
		"altered digest":   recordedCCRReference(config.ModeReadOnly, "ccr-foreground-v7", string(altered)),
		"modeled version":  recordedCCRReference(config.ModeReadOnly, "ccr-foreground-v7-modeled", ccrProjectionReadDigest),
		"other mode label": recordedCCRReference(config.ModeWorkspaceWrite, "ccr-foreground-v7", ccrProjectionReadDigest),
	}
}

// TestCCRProjectionRevisionReplaysItsRecordedPolicy resolves the recorded
// reference to a replay-only interpreter that frames follow-up answers but
// keeps a failed final request malformed and never reports a quota, while the
// current reference keeps its corrected classification.
func TestCCRProjectionRevisionReplaysItsRecordedPolicy(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	if current := Reference(config.ModeReadOnly); current != recordedCCRReference(config.ModeReadOnly, "ccr-foreground-v7", ccrCurrentReadDigest) {
		t.Fatalf("current CCR reference changed: %+v", current)
	}
	recorded := recordedCCRReference(config.ModeReadOnly, "ccr-foreground-v7", ccrProjectionReadDigest)
	frozen, err := registry.Resolve(recorded)
	if err != nil {
		t.Fatalf("recorded CCR projection reference is not registered: %v", err)
	}
	if _, err = registry.ResolveRecovery(recorded); !errors.Is(err, predicate.ErrRecoveryUnsupported) {
		t.Fatalf("CCR projection revision became a recovery source: %v", err)
	}
	if got, answer := evaluateCCRTrace(t, frozen, ccrFollowUpTrace()); got.Verdict != task.VerdictCommitted || answer != ccrFollowUpPayload {
		t.Fatalf("CCR projection revision did not frame follow-up answers: %+v %q", got, answer)
	}
	current, err := registry.Resolve(Reference(config.ModeReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		interpreter predicate.Interpreter
		exitCode    int
		want        string
	}{
		{frozen, 0, "invalid_output"},
		{frozen, 1, "provider_failed"},
		{current, 0, "provider_quota_exhausted"},
		{current, 1, "provider_quota_exhausted"},
	} {
		requireCCRFailedRequestClass(t, test.interpreter, test.exitCode, test.want)
	}
	for name, changed := range ccrProjectionNearMisses() {
		if _, err = registry.Resolve(changed); err == nil {
			t.Fatalf("%s reference resolved", name)
		}
	}
}

// requireCCRFailedRequestClass evaluates the sealed quota failure under
// interpreter with exitCode and requires a rejection whose first diagnostic
// code is want.
func requireCCRFailedRequestClass(t *testing.T, interpreter predicate.Interpreter, exitCode int, want string) {
	t.Helper()
	input, raw := credentialInput(t, interpreter.Reference(), exitCode, ccrQuotaTrace(), "")
	input.ExpectedSession = task.SessionExpectation{Required: true, ID: ccrAttributionSession}
	var answer bytes.Buffer
	got, err := interpreter.Evaluate(input, raw, &answer)
	if err != nil || got.Verdict != task.VerdictRejected || answer.Len() != 0 || len(got.Diagnostics) == 0 || got.Diagnostics[0].Code != want {
		t.Fatalf("%s exit %d: %+v err=%v, want %s", interpreter.Reference().SHA256, exitCode, got, err, want)
	}
}

// TestCCRProjectionPredecessorContinuesUnderCurrentRoute requires the recorded
// projection predecessor to continue under the current route-preserving
// reference with the caller's launch, to reconstruct with its own reference
// and recorded launch, and to keep refusing a model override. Every near miss
// is refused by both paths.
func TestCCRProjectionPredecessorContinuesUnderCurrentRoute(t *testing.T) {
	recorded := recordedCCRReference(config.ModeReadOnly, "ccr-foreground-v7", ccrProjectionReadDigest)
	current := recordedCCRReference(config.ModeReadOnly, "ccr-foreground-v7", ccrCurrentReadDigest)
	requireSuccessorSelectsTarget(t, successorRequest(t, config.ModeReadOnly), recorded, current, true)
	predecessor := successorRequest(t, config.ModeReadOnly)
	predecessor.PriorSession = nil
	if err := validateContinuation(predecessor, task.MetaRecord{Predicate: recorded}, predecessor); err != nil {
		t.Fatalf("same-model continuation refused: %v", err)
	}
	changed := predecessor
	changed.RequestedConfig.Model = "changed-alias"
	if err := validateContinuation(predecessor, task.MetaRecord{Predicate: recorded}, changed); !errors.Is(err, commonprovider.ErrUnsupportedOption) {
		t.Fatalf("route-preserving predecessor model change error=%v, want %v", err, commonprovider.ErrUnsupportedOption)
	}
	for name, reference := range ccrProjectionNearMisses() {
		t.Run(name, func(t *testing.T) {
			for _, mode := range successorModes {
				request := successorRequest(t, mode)
				meta := successorMeta(request, reference)
				if _, err := PrepareSuccessorCandidate(request, meta, os.Environ()); err == nil {
					t.Fatalf("%s successor accepted %+v", mode, reference)
				}
				if _, err := PrepareExistingCandidate(request, meta, os.Environ()); err == nil {
					t.Fatalf("%s reconstruction accepted %+v", mode, reference)
				}
			}
		})
	}
	writeRequest := successorRequest(t, config.ModeWorkspaceWrite)
	if _, err := PrepareSuccessorCandidate(writeRequest, successorMeta(writeRequest, recorded), os.Environ()); err == nil {
		t.Fatal("workspace-write successor accepted the read-only projection reference")
	}
}
