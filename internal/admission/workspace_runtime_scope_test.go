package admission

import (
	"os"
	"path/filepath"
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The probe supervisor client records the TMPDIR it was launched with, a
// control-allowlisted value the bound resolution never overrides.
const tmpdirProbeScript = `#!/bin/sh
printf '%s' "${TMPDIR-unset}" > "$(dirname "$0")/tmpdir-seen"
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  *) exit 64 ;;
esac
`

func TestWorkspaceRuntimeOpenSavedUsesSelectedScopeNotProcessEnvironment(t *testing.T) {
	base := supervisorfixture.CanonicalTempDir(t)
	executable := filepath.Join(base, "pueue")
	if err := os.WriteFile(executable, []byte(tmpdirProbeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "pueue.json")
	admissionfixture.WriteUncertainGroupSupervisorConfig(t, base, configPath, "scope", filepath.Join(base, "status.json"), filepath.Join(base, "status-after.json"))
	path := os.Getenv("PATH")
	bound, err := pueue.Bind(t.Context(), executable, configPath, pueue.Options{Environment: []string{"PATH=" + path}})
	if err != nil {
		t.Fatalf("bind probe supervisor: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := bound.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})

	selectedTmp, ambientTmp := filepath.Join(base, "selected-tmp"), filepath.Join(base, "ambient-tmp")
	t.Setenv("TMPDIR", ambientTmp)
	seen := filepath.Join(base, "tmpdir-seen")
	if err = os.Remove(seen); err != nil {
		t.Fatalf("clear bind-time probe: %v", err)
	}
	runtime := NewWorkspaceRuntime(supervisorcore.AdmissionOptions{Environment: []string{"PATH=" + path, "TMPDIR=" + selectedTmp}})
	store, err := taskdir.InitStore(filepath.Join(base, "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	client, err := runtime.OpenSaved(t.Context(), store, bound.Binding(), infrastructure.SavedBindingSource{})
	if err != nil {
		t.Fatalf("open saved supervisor: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// The probe refuses every non-version command; only the launch environment it saw matters.
	if _, err = client.Snapshot(t.Context()); err == nil {
		t.Fatal("probe supervisor unexpectedly produced a queue snapshot")
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("probe was not launched: %v", err)
	}
	if string(got) != selectedTmp {
		t.Fatalf("supervisor control launched with TMPDIR=%q, want selected %q (ambient %q)", got, selectedTmp, ambientTmp)
	}
}
