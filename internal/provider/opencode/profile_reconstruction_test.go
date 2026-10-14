package opencode

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestExistingNativeProfilePreservesPersistedPredicate(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			for _, revision := range []struct {
				name string
				ref  task.PredicateRef
			}{
				{name: "v2", ref: historicalReferenceForMode(mode)},
				{name: "v3", ref: historicalCurrentReferenceForMode(mode)},
				{name: "v4", ref: historicalCurrentV4ReferenceForMode(mode)},
			} {
				t.Run(revision.name, func(t *testing.T) {
					testPersistedProfile(t, mode, revision.ref)
				})
			}
		})
	}
}

func TestCurrentNativeProfileAdmitsWithoutRuntimeFacts(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest(ModeReadOnly)
	request.CanonicalCwd = directory
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	static := requireStaticAdmission(t, candidate)
	assertFinalizedAdmission(t, candidate, static)
}

func requireStaticAdmission(t *testing.T, candidate commonprovider.ProfileCandidate) commonprovider.PreparedProfile {
	t.Helper()
	if candidate.Admit == nil {
		t.Fatal("current native candidate does not expose static admission")
	}
	static, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if static.ObservedVersion != "" || static.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{}) {
		t.Fatalf("static admission claimed runtime facts: version=%q runtime=%+v", static.ObservedVersion, static.Plan.ProviderRuntime)
	}
	err = task.ValidateSHA256(static.RuntimePreflightSHA256)
	if err != nil {
		t.Fatalf("static capability digest=%q: %v", static.RuntimePreflightSHA256, err)
	}
	if candidate.Inspection == nil {
		t.Fatal("current native candidate omitted runtime inspection")
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	if static.RuntimePreflightSHA256 != capability {
		t.Fatalf("static capability digest=%q want=%q", static.RuntimePreflightSHA256, capability)
	}
	if static.Plan.RuntimePreflightSHA256 != "" {
		t.Fatalf("provider static admission populated plan marker=%q", static.Plan.RuntimePreflightSHA256)
	}
	return static
}

func assertFinalizedAdmission(t *testing.T, candidate commonprovider.ProfileCandidate, static commonprovider.PreparedProfile) {
	t.Helper()
	if _, err := candidate.Finalize(nil, time.Now()); err == nil {
		t.Fatal("finalizer accepted missing runtime facts")
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable, Version: "OpenCode fixture", SHA256: candidate.Inspection.ExecutableSHA256,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if finalized.RuntimePreflightSHA256 != "" || finalized.ObservedVersion == "" {
		t.Fatalf("finalized profile retained pending runtime state: %+v", finalized)
	}
	if !slices.Equal(static.Plan.Arguments, finalized.Plan.Arguments) ||
		!slices.Equal(static.Plan.Environment, finalized.Plan.Environment) ||
		!task.CompareEffectiveConfigs(static.Effective, finalized.Effective) {
		t.Fatalf("static and finalized launch contracts differ: static=%+v finalized=%+v", static, finalized)
	}
}

func TestPendingCurrentNativeProfileReconstructsStaticAdmission(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest(ModeReadOnly)
	request.CanonicalCwd = directory
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PrepareExistingCandidate(request, task.MetaRecord{
		EffectiveConfig:        admitted.Effective,
		RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment:            slices.Clone(admitted.Plan.Environment),
		EnvironmentRecorded:    true,
		Predicate:              admitted.Plan.Predicate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Admit == nil {
		t.Fatal("pending current native profile lost static admission")
	}
	rebuilt, err := pending.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ObservedVersion != "" || rebuilt.RuntimePreflightSHA256 != admitted.RuntimePreflightSHA256 {
		t.Fatalf("pending reconstruction claimed unexpected runtime state: rebuilt=%+v admitted=%+v", rebuilt, admitted)
	}
	if !slices.Equal(rebuilt.Plan.Arguments, admitted.Plan.Arguments) ||
		!slices.Equal(rebuilt.Plan.Environment, admitted.Plan.Environment) ||
		!task.CompareEffectiveConfigs(rebuilt.Effective, admitted.Effective) {
		t.Fatalf("pending reconstruction changed static launch contract: rebuilt=%+v admitted=%+v", rebuilt, admitted)
	}
}

func testPersistedProfile(t *testing.T, mode string, reference task.PredicateRef) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest(mode)
	request.CanonicalCwd = directory
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	existing, err := PrepareExistingCandidate(request, task.MetaRecord{Predicate: reference, EffectiveConfig: current.Effective})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := existing.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !current.Plan.Predicate.Equal(ReferenceForMode(mode)) || !rebuilt.Plan.Predicate.Equal(reference) || !slices.Equal(current.Plan.Arguments, rebuilt.Plan.Arguments) || !task.CompareEffectiveConfigs(current.Effective, rebuilt.Effective) {
		t.Fatalf("predicate or launch contract changed: current=%+v rebuilt=%+v", current.Plan.Predicate, rebuilt.Plan.Predicate)
	}
}
