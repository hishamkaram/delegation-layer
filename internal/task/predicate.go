package task

import "fmt"

// fixturePredicateV1Contract is the checked-in immutable interpretation contract.
// Its exact bytes define the digest. Changing any rule requires a new version
// and retaining this implementation when recovery of v1 remains supported.
const fixturePredicateV1Contract = `{"adapter":"fixture:test","mode":"read-only","version":"1","raw":["raw/stderr","raw/stdout"],"success":"started,exit_code=0,error=empty,stdout-size>0; preserve every stdout byte","refusals":{"start_failed":"start_failed: <error>","error":"capture_error: <error>","nonzero_exit":"provider_exit: <decimal-exit-code>","empty_stdout":"empty_answer"},"precedence":["start_failed","error","nonzero_exit","empty_stdout","success"]}` + "\n"

// FixturePredicateRef returns the one explicitly registered Phase 1 predicate.
// Native provider predicates will be registered by their later adapter phases.
func FixturePredicateRef() PredicateRef {
	return PredicateRef{
		Adapter: "fixture:test",
		Mode:    "read-only",
		Version: "1",
		SHA256:  ComputeSHA256([]byte(fixturePredicateV1Contract)),
	}
}

// PredicateDecision selects a validated raw file or deterministic refusal bytes.
// The publisher must verify the seal's entire evidence set before evaluation
// and stream RawPath through its rooted store, never trust a caller's payload.
type PredicateDecision struct {
	Verdict         string
	PayloadBasename string
	RawPath         string
	PayloadContent  []byte
}

// EvaluateRegisteredPredicate is a pure interpretation of validated seal fields.
// Exact raw bytes are verified separately by taskdir and never loaded here.
func EvaluateRegisteredPredicate(ref PredicateRef, seal *ProviderExitRecord) (PredicateDecision, error) {
	if !ref.Equal(FixturePredicateRef()) {
		return PredicateDecision{}, ErrIncompatiblePredicate
	}
	if seal == nil {
		return PredicateDecision{}, ErrNoSeal
	}
	if err := ValidateProviderExitRecord(seal); err != nil {
		return PredicateDecision{}, err
	}
	if !ref.Equal(seal.Predicate) {
		return PredicateDecision{}, ErrIdentityMismatch
	}
	if reason := fixtureRefusal(seal); reason != "" {
		return PredicateDecision{Verdict: VerdictRejected, PayloadBasename: "publish.reject", PayloadContent: []byte(reason)}, nil
	}
	return PredicateDecision{Verdict: VerdictCommitted, PayloadBasename: "result.txt", RawPath: "raw/stdout"}, nil
}

func fixtureRefusal(seal *ProviderExitRecord) string {
	if seal.InvocationState == InvocationStartFailed {
		return "start_failed: " + seal.Error
	}
	if seal.Error != "" {
		return "capture_error: " + seal.Error
	}
	if seal.ExitCode != 0 {
		return fmt.Sprintf("provider_exit: %d", seal.ExitCode)
	}
	// The validator guarantees this strictly ordered complete manifest.
	if seal.RawManifest[1].Size == 0 {
		return "empty_answer"
	}
	return ""
}
