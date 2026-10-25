package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOptionsForEnvironmentSnapshotsSelectedScope(t *testing.T) {
	t.Setenv("HOME", "/ambient-home")
	scope := []string{"HOME=/selected-home", "PATH=/selected-bin", "SECRET_TOKEN=hidden"}
	defaulted := OptionsForEnvironment(pueue.Options{}, scope)
	if want := []string{"HOME=/selected-home", "PATH=/selected-bin"}; !slices.Equal(defaulted.Environment, want) {
		t.Fatalf("default control environment = %q, want %q", defaulted.Environment, want)
	}
	if empty := OptionsForEnvironment(pueue.Options{}, []string{}); empty.Environment == nil || len(empty.Environment) != 0 {
		t.Fatalf("empty scope collapsed to the process environment: %#v", empty.Environment)
	}

	explicit := []string{"HOME=/explicit-home"}
	selected := OptionsForEnvironment(pueue.Options{Environment: explicit}, scope)
	if !slices.Equal(selected.Environment, explicit) {
		t.Fatalf("explicit control environment was replaced: %q", selected.Environment)
	}
	selected.Environment[0] = "HOME=/mutated"
	if explicit[0] != "HOME=/explicit-home" {
		t.Fatal("selected control environment aliases its base")
	}
	explicitEmpty := OptionsForEnvironment(pueue.Options{Environment: []string{}}, scope)
	if explicitEmpty.Environment == nil || len(explicitEmpty.Environment) != 0 {
		t.Fatalf("explicit empty control environment changed: %#v", explicitEmpty.Environment)
	}
}

func TestOptionsForEnvironmentDefaultsMaintenanceOnlyForCapturedScope(t *testing.T) {
	t.Setenv("LANG", "ambient")
	scope := []string{"LANG=selected", "SECRET_TOKEN=hidden"}
	if got := OptionsForEnvironment(pueue.Options{}, scope).PrivateMaintenanceEnvironment; !slices.Equal(got, []string{"LANG=selected"}) {
		t.Fatalf("captured maintenance environment = %q, want the bounded selected scope", got)
	}
	uncaptured := OptionsForEnvironment(pueue.Options{}, nil)
	if uncaptured.PrivateMaintenanceEnvironment != nil {
		t.Fatalf("nil scope selected maintenance environment %q, want standalone default", uncaptured.PrivateMaintenanceEnvironment)
	}
	if uncaptured.Environment == nil || len(uncaptured.Environment) != 0 {
		t.Fatalf("nil scope control environment = %#v, want explicit empty", uncaptured.Environment)
	}
	if empty := OptionsForEnvironment(pueue.Options{}, []string{}).PrivateMaintenanceEnvironment; empty == nil || len(empty) != 0 {
		t.Fatalf("empty scope maintenance environment = %#v, want explicit empty", empty)
	}
	explicit := []string{"LANG=explicit"}
	selected := OptionsForEnvironment(pueue.Options{PrivateMaintenanceEnvironment: explicit}, scope)
	if !slices.Equal(selected.PrivateMaintenanceEnvironment, explicit) {
		t.Fatalf("explicit maintenance environment was replaced: %q", selected.PrivateMaintenanceEnvironment)
	}
	selected.PrivateMaintenanceEnvironment[0] = "LANG=mutated"
	if explicit[0] != "LANG=explicit" {
		t.Fatal("selected maintenance environment aliases its base")
	}
	if kept := OptionsForEnvironment(pueue.Options{PrivateMaintenanceEnvironment: []string{}}, scope).PrivateMaintenanceEnvironment; kept == nil || len(kept) != 0 {
		t.Fatalf("explicit empty maintenance environment changed: %#v", kept)
	}
}

func TestAdmissionOptionsForScopeSeparatesControlFromMaintenanceScope(t *testing.T) {
	t.Setenv("HOME", "/ambient-home")
	t.Setenv("PATH", "/ambient-bin")
	ambient := os.Environ()
	control := []string{"HOME=/caller-home", "PATH=/caller-bin"}
	scope := []string{"HOME=/recorded-home", "PATH=/recorded-bin", "PROVIDER_ONLY=value"}
	base := AdmissionOptions{InitialExecutable: "/fixed/pueue"}

	selected := AdmissionOptionsForScope(base, control, scope)
	if !slices.Equal(selected.Control.Environment, control) {
		t.Fatalf("control environment = %q, want caller %q", selected.Control.Environment, control)
	}
	if want := []string{"HOME=/recorded-home", "PATH=/recorded-bin"}; !slices.Equal(selected.Control.PrivateMaintenanceEnvironment, want) {
		t.Fatalf("maintenance environment = %q, want %q", selected.Control.PrivateMaintenanceEnvironment, want)
	}
	if lookup := selected.Lookup(); lookup.InitialExecutable != "/fixed/pueue" || !slices.Equal(lookup.Environment, scope) {
		t.Fatalf("executable lookup = %+v, want selected scope", lookup)
	}
	scope[1] = "PATH=/mutated"
	if selected.Environment[1] != "PATH=/recorded-bin" {
		t.Fatal("admission scope aliases the caller's slice")
	}
	if base.Environment != nil || base.Control.Environment != nil || base.Control.PrivateMaintenanceEnvironment != nil {
		t.Fatalf("scope selection changed its base: %+v", base)
	}
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("scope selection changed the process environment")
	}
}

func TestInitialConfigPrefersExplicitConfigOverSelectedScope(t *testing.T) {
	t.Setenv(environmentConfigKey, "/ambient/pueue.yml")
	tests := []struct {
		name      string
		selection AdmissionSelection
		want      string
		err       error
	}{
		{name: "explicit wins", selection: AdmissionSelection{ConfigPath: "/explicit/pueue.yml", EnvironmentConfigPath: "/scope/pueue.yml"}, want: "/explicit/pueue.yml"},
		{name: "scope fallback", selection: AdmissionSelection{EnvironmentConfigPath: "/scope/pueue.yml"}, want: "/scope/pueue.yml"},
		{name: "ambient ignored", selection: AdmissionSelection{}, want: ""},
		{name: "relative scope refused", selection: AdmissionSelection{EnvironmentConfigPath: "relative/pueue.yml"}, err: pueue.ErrConfiguration},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveInitialConfig(test.selection)
			if !errors.Is(err, test.err) || got != test.want {
				t.Fatalf("ResolveInitialConfig = %q/%v, want %q/%v", got, err, test.want, test.err)
			}
		})
	}
}

func TestContinuationConfigEnvironmentSuppressesPrivatePredecessor(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	private := pueue.PrivateConfigPath(root)
	scope := []string{environmentConfigKey + "=/first/pueue.yml", environmentConfigKey + "=/second/pueue.yml"}
	if got := EnvironmentConfigPath(scope); got != "/first/pueue.yml" {
		t.Fatalf("first scope entry did not win: %q", got)
	}
	if got := EnvironmentConfigPath([]string{"PATH=/bin"}); got != "" {
		t.Fatalf("absent scope config = %q", got)
	}
	if got := EnvironmentConfigPath([]string{environmentConfigKey, environmentConfigKey + "=", environmentConfigKey + "=/later/pueue.yml"}); got != "" {
		t.Fatalf("empty first scope config did not win over a later entry: %q", got)
	}
	t.Setenv(environmentConfigKey, "/ambient/pueue.yml")
	if got := EnvironmentConfigPath(nil); got != "" {
		t.Fatalf("nil scope config read the process environment: %q", got)
	}
	privatePredecessor := task.MetaRecord{SupervisorConfig: task.SupervisorRef{ConfigPath: private}}
	if got := ContinuationConfigEnvironment(root, privatePredecessor, scope); got != "" {
		t.Fatalf("private predecessor inherited external config %q", got)
	}
	externalPredecessor := task.MetaRecord{SupervisorConfig: task.SupervisorRef{ConfigPath: "/external/pueue.yml"}}
	if got := ContinuationConfigEnvironment(root, externalPredecessor, scope); got != "/first/pueue.yml" {
		t.Fatalf("external predecessor config = %q", got)
	}
}

func TestSupervisorLookupResolvesPathFromSelectedScope(t *testing.T) {
	ambientDirectory := writeScopeSupervisorPair(t, "ambient")
	selectedDirectory := writeScopeSupervisorPair(t, "selected")
	t.Setenv("PATH", ambientDirectory)
	ambient := os.Environ()

	selected := ExecutableLookup{Environment: []string{"PATH=" + selectedDirectory}}
	initial, err := ResolveInitialExecutable(selected)
	if err != nil || initial != filepath.Join(selectedDirectory, "pueue") {
		t.Fatalf("initial executable = %q/%v, want selected scope", initial, err)
	}
	client, daemon, err := ResolvePrivateSupervisorExecutables(supervisorfixture.CanonicalTempDir(t), selected)
	if err != nil || client != filepath.Join(selectedDirectory, "pueue") || daemon != filepath.Join(selectedDirectory, "pueued") {
		t.Fatalf("private PATH fallback = %q/%q/%v, want selected scope", client, daemon, err)
	}
	if _, err = ResolveInitialExecutable(ExecutableLookup{Environment: []string{}}); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("empty scope resolved through the process PATH: %v", err)
	}
	standalone, err := ResolveInitialExecutable(ExecutableLookup{})
	if err != nil || standalone != filepath.Join(ambientDirectory, "pueue") {
		t.Fatalf("standalone lookup = %q/%v, want process PATH", standalone, err)
	}
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("scoped lookup changed the process environment")
	}
}

func writeScopeSupervisorPair(t *testing.T, label string) string {
	t.Helper()
	directory := supervisorfixture.CanonicalTempDir(t)
	for _, name := range []string{"pueue", "pueued"} {
		supervisorfixture.WriteExecutable(t, filepath.Join(directory, name), label+" "+name)
	}
	return directory
}
