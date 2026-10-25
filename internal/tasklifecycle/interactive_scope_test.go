package tasklifecycle

import (
	"os"
	"slices"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	interactiveScopeCaller   = []string{"HOME=/caller-home", "PATH=/caller-bin", "DELEGATE_PUEUE_CONFIG=/caller/pueue.yml"}
	interactiveScopeRecorded = []string{"HOME=/recorded-home", "PATH=/recorded-bin", "DELEGATE_PUEUE_CONFIG=/recorded/pueue.yml"}
	interactiveScopeConfig   = supervisorcore.AdmissionSelection{ConfigPath: "/explicit/pueue.yml", EnvironmentConfigPath: "/caller/pueue.yml"}
)

// interactiveScopeFixture returns caller-scoped creation options and an owned
// state root for one interactive scope selection.
func interactiveScopeFixture(t *testing.T) (CreationOptions, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "dl-scope-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(root); removeErr != nil {
			t.Error(removeErr)
		}
	})
	return CreationOptions{Admission: admissioncore.PreparationOptions{
		Profiles:   admissioncore.NewProfiles(nil, nil, interactiveScopeCaller),
		Supervisor: supervisorcore.AdmissionOptions{InitialExecutable: "/fixed/pueue"},
	}}, root
}

func TestInteractiveContinuationCreationKeepsCallerScopeOutsideInteractiveResume(t *testing.T) {
	options, root := interactiveScopeFixture(t)
	external := &task.MetaRecord{Environment: interactiveScopeRecorded, EnvironmentRecorded: true, SupervisorConfig: task.SupervisorRef{ConfigPath: "/external/pueue.yml"}}
	for _, test := range []struct {
		name        string
		meta        *task.MetaRecord
		interactive bool
	}{
		{name: "ordinary resume", meta: external, interactive: false},
		{name: "fresh dispatch", meta: nil, interactive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			kept, keptSelection, err := interactiveContinuationCreation(options, interactiveScopeConfig, root, test.meta, test.interactive)
			if err != nil || !slices.Equal(kept.Admission.Profiles.Environment(), interactiveScopeCaller) || keptSelection != interactiveScopeConfig || kept.Admission.Supervisor.Environment != nil {
				t.Fatalf("caller scope changed: %q/%+v/%v", kept.Admission.Profiles.Environment(), keptSelection, err)
			}
		})
	}
}

func TestInteractiveContinuationCreationSelectsPredecessorScope(t *testing.T) {
	ambient := os.Environ()
	options, root := interactiveScopeFixture(t)
	external := &task.MetaRecord{Environment: interactiveScopeRecorded, EnvironmentRecorded: true, SupervisorConfig: task.SupervisorRef{ConfigPath: "/external/pueue.yml"}}
	selected, selection, err := interactiveContinuationCreation(options, interactiveScopeConfig, root, external, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(selected.Admission.Profiles.Environment(), interactiveScopeRecorded) || !slices.Equal(selected.Admission.Supervisor.Environment, interactiveScopeRecorded) {
		t.Fatalf("interactive scope = %q / %q, want recorded", selected.Admission.Profiles.Environment(), selected.Admission.Supervisor.Environment)
	}
	control := selected.Admission.Supervisor.Control
	if want := []string{"HOME=/recorded-home", "PATH=/recorded-bin"}; !slices.Equal(control.Environment, want) || !slices.Equal(control.PrivateMaintenanceEnvironment, want) {
		t.Fatalf("interactive control/maintenance = %q / %q", control.Environment, control.PrivateMaintenanceEnvironment)
	}
	if selection.ConfigPath != "/explicit/pueue.yml" || selection.EnvironmentConfigPath != "/recorded/pueue.yml" {
		t.Fatalf("interactive config selection = %+v", selection)
	}
	if !slices.Equal(options.Admission.Profiles.Environment(), interactiveScopeCaller) || options.Admission.Supervisor.Environment != nil {
		t.Fatal("interactive selection changed the caller's options")
	}
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("interactive selection changed the process environment")
	}
}

func TestInteractiveContinuationCreationPrivateAndInvalidScopes(t *testing.T) {
	options, root := interactiveScopeFixture(t)
	private := &task.MetaRecord{Environment: interactiveScopeRecorded, EnvironmentRecorded: true, SupervisorConfig: task.SupervisorRef{ConfigPath: pueue.PrivateConfigPath(root)}}
	_, selection, err := interactiveContinuationCreation(options, interactiveScopeConfig, root, private, true)
	if err != nil || selection.EnvironmentConfigPath != "" || selection.ConfigPath != "/explicit/pueue.yml" {
		t.Fatalf("private predecessor kept scope config: %+v/%v", selection, err)
	}
	invalid := &task.MetaRecord{Environment: []string{"PATH=/a", "PATH=/b"}, EnvironmentRecorded: true}
	if _, _, err = interactiveContinuationCreation(options, interactiveScopeConfig, root, invalid, true); err == nil {
		t.Fatal("interactive creation accepted an invalid saved scope")
	}
}
