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
			} {
				t.Run(revision.name, func(t *testing.T) {
					testPersistedProfile(t, mode, revision.ref)
				})
			}
		})
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
