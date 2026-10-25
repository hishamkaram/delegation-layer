package app

import (
	"os"
	"path/filepath"
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// modelsTMPDIRProbeScript records the TMPDIR of every supervisor control
// launch, a control-allowlisted value the bound resolution never overrides.
const modelsTMPDIRProbeScript = `#!/bin/sh
printf '%s' "${TMPDIR-unset}" > "$(dirname "$0")/tmpdir-seen"
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  *) exit 64 ;;
esac
`

func TestModelDiscoverySupervisorControlUsesSelectedScope(t *testing.T) {
	base := testsupport.CanonicalTempDir(t)
	selectedTmp, ambientTmp := filepath.Join(base, "selected-tmp"), filepath.Join(base, "ambient-tmp")
	for _, directory := range []string{selectedTmp, ambientTmp} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	probe, configPath := filepath.Join(base, "pueue"), filepath.Join(base, "pueue.json")
	if err := os.WriteFile(probe, []byte(modelsTMPDIRProbeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	admissionfixture.WriteUncertainGroupSupervisorConfig(t, base, configPath, "scope", filepath.Join(base, "status.json"), filepath.Join(base, "status-after.json"))
	runner := filepath.Join(base, "delegate-run")
	writeRunnerFixture(t, runner, "runner")
	store := testsupport.NewInspectionStore(t)
	workspace := testsupport.CanonicalTempDir(t)
	environment := []string{"PROVIDER_PROFILE=fixture"}
	digest := task.ComputeSHA256([]byte("fake"))
	definition := commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: "/fake/provider", ExecutableSHA256: digest,
		Directory: workspace, Environment: environment, OutputLimit: commonprovider.MaxInspectionOutput,
		Runtime: &commonprovider.RuntimeProbeDefinition{Executable: "/fake/provider", ExecutableSHA256: digest, Directory: workspace, Environment: environment, RequiredFlags: []string{"--fixture"}},
	}
	deps := Dependencies{
		Environment:                 []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + selectedTmp},
		InitialSupervisorExecutable: probe,
	}
	t.Setenv("TMPDIR", ambientTmp)

	_, supervisor, _, err := prepareModelDiscoverySupervisor(Arguments{PueueConfig: configPath, Runner: runner}, deps, store, commonprovider.ProfileCandidate{Directory: workspace, Inspection: &definition})
	if supervisor != nil {
		t.Cleanup(func() {
			if closeErr := supervisor.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	seen, readErr := os.ReadFile(filepath.Join(base, "tmpdir-seen"))
	if readErr != nil {
		t.Fatalf("probe supervisor was not launched (discovery error %v): %v", err, readErr)
	}
	if string(seen) != selectedTmp {
		t.Fatalf("model discovery supervisor control launched with TMPDIR=%q, want selected %q (ambient %q)", seen, selectedTmp, ambientTmp)
	}
}
