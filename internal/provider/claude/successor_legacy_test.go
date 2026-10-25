package claude

import (
	"errors"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// legacyPolicyFixture is one legacy native OAuth finalization: a caller's
// runtime, its inspection definition digest, and its observed policy facts.
// These are fixtures for the provider-owned finalizer; they make no claim
// about the macOS Keychain helper or a real Claude login.
type legacyPolicyFixture struct {
	cli              commonprovider.CLIInfo
	definitionSHA256 string
	expiresIn        time.Duration
}

func (f legacyPolicyFixture) finalize(t *testing.T, request task.TaskRecord, now time.Time, historicalProof ...*task.PolicySourceDigest) (commonprovider.PreparedProfile, error) {
	t.Helper()
	arguments, inputs, err := legacyPrintArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	native, err := projectNativePolicy(nativePolicyFixture(t, "team", now.Add(f.expiresIn).UnixMilli()), 404, nil)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: f.cli.Path, Version: "Claude Code fixture", SHA256: f.cli.SHA256}, native)
	if err != nil {
		t.Fatal(err)
	}
	environment := profileEnvironment{RuntimeSHA256: f.cli.SHA256, WritableRoots: []string{"/home/fixture/.claude"}}
	return finalizeLegacyPreparedProfile(request, arguments, inputs, f.cli, environment, nil, f.definitionSHA256, LegacyReference(), facts, now, historicalProof...)
}

func requireLegacyProof(t *testing.T, label string, profile commonprovider.PreparedProfile) task.PolicySourceDigest {
	t.Helper()
	proof := legacyPolicyProof(task.MetaRecord{EffectiveConfig: profile.Effective})
	if proof == nil {
		t.Fatalf("%s recorded no native OAuth policy proof", label)
	}
	return *proof
}

// TestLegacySuccessorDerivesFreshPolicyProofWhileReconstructionRetainsIt
// pairs the two legacy finalizations: reconstructing admitted record A keeps
// A's recorded proof across a refreshed inspection, while a new successor,
// which passes no recorded proof, binds a fresh proof to its own caller's
// inspection definition and checks the new task's budget and refresh margin.
func TestLegacySuccessorDerivesFreshPolicyProofWhileReconstructionRetainsIt(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	request := profileRequest()
	callerA := legacyPolicyFixture{cli: commonprovider.CLIInfo{Path: "/caller-a/claude", SHA256: strings.Repeat("a", 64)}, definitionSHA256: strings.Repeat("1", 64), expiresIn: time.Hour}
	callerB := legacyPolicyFixture{cli: commonprovider.CLIInfo{Path: "/caller-b/claude", SHA256: strings.Repeat("b", 64)}, definitionSHA256: strings.Repeat("2", 64), expiresIn: time.Hour}

	admitted, err := callerA.finalize(t, request, now)
	if err != nil {
		t.Fatal(err)
	}
	recordedProof := requireLegacyProof(t, "predecessor", admitted)
	predecessor := task.MetaRecord{EffectiveConfig: admitted.Effective, Predicate: admitted.Plan.Predicate}

	refreshed := callerA
	refreshed.definitionSHA256 = strings.Repeat("3", 64)
	reconstructed, err := refreshed.finalize(t, request, now.Add(time.Minute), legacyPolicyProof(predecessor))
	if err != nil {
		t.Fatal(err)
	}
	if got := requireLegacyProof(t, "reconstruction", reconstructed); got != recordedProof {
		t.Fatalf("reconstruction proof %+v want the recorded %+v", got, recordedProof)
	}

	successor, err := callerB.finalize(t, request, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	successorProof := requireLegacyProof(t, "successor", successor)
	if successorProof.SHA256 == recordedProof.SHA256 {
		t.Fatal("successor reused the predecessor's policy proof")
	}
	if successor.Effective.Policy.RuntimeSHA256 != callerB.cli.SHA256 || successor.Plan.Executable != callerB.cli.Path {
		t.Fatal("successor policy is not bound to its caller's runtime")
	}
	otherDefinition := callerB
	otherDefinition.definitionSHA256 = strings.Repeat("4", 64)
	rebound, err := otherDefinition.finalize(t, request, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if requireLegacyProof(t, "rebound successor", rebound).SHA256 == successorProof.SHA256 {
		t.Fatal("successor proof is not bound to the caller's inspection definition")
	}

	// The new task's own budget and the refresh margin bound the caller's
	// credential expiry; the predecessor's proof cannot extend it.
	longer := request
	longer.BudgetNanos = int64(2 * time.Hour)
	for name, historical := range map[string][]*task.PolicySourceDigest{"successor": nil, "with recorded proof": {legacyPolicyProof(predecessor)}} {
		if _, err = callerB.finalize(t, longer, now, historical...); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("%s accepted a credential expiring within its budget: %v", name, err)
		}
	}
	expiring := callerB
	expiring.expiresIn = time.Duration(request.BudgetNanos) + refreshMargin
	if _, err = expiring.finalize(t, request, now); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("successor accepted a credential expiring within the refresh margin: %v", err)
	}
}
