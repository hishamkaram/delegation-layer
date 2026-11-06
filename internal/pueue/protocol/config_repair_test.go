package protocol

import (
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRepairConfigReplacesShellAndEnvironmentWithoutChangingProfiles(t *testing.T) {
	source := []byte("daemon: &daemon\n  shell_command: [/user/shell, '-c', '{{ pueue_command_string }}']\n  env_vars: {EDITOR: unsafe, PUEUE_EDIT_PATH: unsafe}\n  pause_group_on_failure: true\nprofiles:\n  saved:\n    daemon: *daemon\nclient:\n  show_confirmation_questions: false\n")
	shell := []string{"/managed/delegate-run", "--supervisor-repair-editor", "{{ pueue_command_string }}"}
	data, err := ConfigWithRepairEditor(source, shell)
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if config.Client.EditMode != "files" || config.Daemon.ShellCommand == nil || !reflect.DeepEqual(*config.Daemon.ShellCommand, shell) || len(config.Daemon.EnvVars) != 0 || !config.Daemon.PauseGroupOnFailure {
		t.Fatalf("repair config did not isolate its editor: %+v", config)
	}
	var before, after rawSettings
	if err = yaml.Unmarshal(source, &before); err != nil {
		t.Fatal(err)
	}
	if err = yaml.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Profiles, after.Profiles) {
		t.Fatal("derived config mutated a profile")
	}
}

func TestRepairConfigRejectsTemplateInManagedExecutable(t *testing.T) {
	_, err := ConfigWithRepairEditor([]byte("client: {}\n"), []string{"/managed/{{other}}", "--repair", "{{ pueue_command_string }}"})
	if err == nil {
		t.Fatal("managed editor path allowed native template expansion")
	}
}
