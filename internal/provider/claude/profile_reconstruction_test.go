package claude

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestExistingProfilePreservesPreUpgradePredicates(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, interactive := range []bool{false, true} {
			reference := historicalNativeReferenceForMode(mode)
			if interactive {
				reference = historicalInteractionReference(mode)
			}
			t.Run(mode+"/"+reference.Version, func(t *testing.T) {
				testPreUpgradeProfile(t, mode, interactive, reference)
			})
		}
	}
}

func testPreUpgradeProfile(t *testing.T, mode string, interactive bool, reference task.PredicateRef) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "claude"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := profileRequest()
	request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
	request.RequestedConfig.Interactive = interactive
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
	meta := task.MetaRecord{Predicate: reference, EffectiveConfig: current.Effective, Environment: current.Plan.Environment, EnvironmentRecorded: true}
	existing, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := existing.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt.Plan.Predicate.Equal(reference) || !slices.Equal(current.Plan.Arguments, rebuilt.Plan.Arguments) || !task.CompareEffectiveConfigs(current.Effective, rebuilt.Effective) {
		t.Fatalf("predicate or launch contract changed: current=%+v rebuilt=%+v", current.Plan.Predicate, rebuilt.Plan.Predicate)
	}
	// A stored predicate cannot authorize a different permission contract.
	meta.EffectiveConfig.Approval = nativeHistoricalPermission(mode)
	if !interactive {
		if _, err = PrepareExistingCandidate(request, meta); err == nil {
			t.Fatal("historical predicate accepted with incompatible permission mode")
		}
	}
}
