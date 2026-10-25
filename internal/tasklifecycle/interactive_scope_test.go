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

func TestNewSuccessorCreationKeepsFreshDispatchUnchanged(t *testing.T) {
	options, root := interactiveScopeFixture(t)
	kept, keptSelection := newSuccessorCreation(options, interactiveScopeConfig, root, nil)
	if !slices.Equal(kept.Admission.Profiles.Environment(), interactiveScopeCaller) || keptSelection != interactiveScopeConfig || kept.Admission.Supervisor.Environment != nil {
		t.Fatalf("fresh dispatch scope changed: %q/%+v", kept.Admission.Profiles.Environment(), keptSelection)
	}
}

// TestNewSuccessorCreationSelectsCallerScope covers every new successor:
// ordinary and interactive continuations share one selection, so the request
// mode is not an input. The predecessor's saved scope, even an invalid one, is
// compatibility evidence only and never selects the new task's environment.
func TestNewSuccessorCreationSelectsCallerScope(t *testing.T) {
	ambient := os.Environ()
	saved := task.SupervisorRef{ConfigPath: "/saved/pueue.yml", Endpoint: "unix:/saved/pueue.sock"}
	selectionInput := interactiveScopeConfig
	selectionInput.SavedSupervisor = &saved
	for _, test := range []struct {
		name string
		meta *task.MetaRecord
	}{
		{name: "external predecessor", meta: &task.MetaRecord{Environment: interactiveScopeRecorded, EnvironmentRecorded: true, SupervisorConfig: task.SupervisorRef{ConfigPath: "/external/pueue.yml"}}},
		{name: "invalid saved predecessor scope", meta: &task.MetaRecord{Environment: []string{"PATH=/a", "PATH=/b"}, EnvironmentRecorded: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, root := interactiveScopeFixture(t)
			selected, selection := newSuccessorCreation(options, selectionInput, root, test.meta)
			if !slices.Equal(selected.Admission.Profiles.Environment(), interactiveScopeCaller) || !slices.Equal(selected.Admission.Supervisor.Environment, interactiveScopeCaller) {
				t.Fatalf("successor scope = %q / %q, want caller", selected.Admission.Profiles.Environment(), selected.Admission.Supervisor.Environment)
			}
			control := selected.Admission.Supervisor.Control
			if want := []string{"HOME=/caller-home", "PATH=/caller-bin"}; !slices.Equal(control.Environment, want) || !slices.Equal(control.PrivateMaintenanceEnvironment, want) {
				t.Fatalf("successor control/maintenance = %q / %q", control.Environment, control.PrivateMaintenanceEnvironment)
			}
			if selection.ConfigPath != "/explicit/pueue.yml" || selection.EnvironmentConfigPath != "/caller/pueue.yml" || selection.SavedSupervisor == nil || *selection.SavedSupervisor != saved {
				t.Fatalf("successor config selection = %+v", selection)
			}
			if !slices.Equal(options.Admission.Profiles.Environment(), interactiveScopeCaller) || options.Admission.Supervisor.Environment != nil {
				t.Fatal("successor selection changed the caller's options")
			}
			if !slices.Equal(os.Environ(), ambient) {
				t.Fatal("successor selection changed the process environment")
			}
		})
	}
}

func TestNewSuccessorCreationSuppressesPrivatePredecessorConfig(t *testing.T) {
	options, root := interactiveScopeFixture(t)
	private := &task.MetaRecord{Environment: interactiveScopeRecorded, EnvironmentRecorded: true, SupervisorConfig: task.SupervisorRef{ConfigPath: pueue.PrivateConfigPath(root)}}
	_, selection := newSuccessorCreation(options, interactiveScopeConfig, root, private)
	if selection.EnvironmentConfigPath != "" || selection.ConfigPath != "/explicit/pueue.yml" {
		t.Fatalf("private predecessor kept the caller scope config: %+v", selection)
	}
}

func TestNewSuccessorCreationKeepsExplicitControlEnvironment(t *testing.T) {
	options, root := interactiveScopeFixture(t)
	explicit := []string{"HOME=/control-home"}
	options.Admission.Supervisor.Control = pueue.Options{Environment: explicit}
	external := &task.MetaRecord{Environment: interactiveScopeRecorded, EnvironmentRecorded: true, SupervisorConfig: task.SupervisorRef{ConfigPath: "/external/pueue.yml"}}
	selected, _ := newSuccessorCreation(options, interactiveScopeConfig, root, external)
	control := selected.Admission.Supervisor.Control
	if !slices.Equal(control.Environment, explicit) || !slices.Equal(control.PrivateMaintenanceEnvironment, []string{"HOME=/caller-home", "PATH=/caller-bin"}) {
		t.Fatalf("explicit control/maintenance = %q / %q", control.Environment, control.PrivateMaintenanceEnvironment)
	}
}
