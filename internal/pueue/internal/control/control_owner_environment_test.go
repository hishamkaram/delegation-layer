package control

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	ownerMarkerKey      = "LANG"
	ownerInheritedValue = "issue91-inherited-marker"
	ownerSuppliedValue  = "issue91-supplied-marker"
)

func TestControlOwnerChildEnvironmentFollowsCommandEnvironment(t *testing.T) {
	t.Setenv(ownerMarkerKey, ownerInheritedValue)
	for _, tc := range []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "nil inherits the caller environment", environment: nil, want: ownerInheritedValue},
		{name: "explicit empty stays empty", environment: []string{}, want: "unset"},
		{name: "supplied values pass through", environment: []string{ownerMarkerKey + "=" + ownerSuppliedValue}, want: ownerSuppliedValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable := finiteExecutable(t, "printf '%s' \"${"+ownerMarkerKey+"-unset}\"")
			command := exec.Command(executable)
			command.Dir = filepath.Dir(executable)
			command.Env = tc.environment
			pending := startControlOwnerTest(t, command, "ffffffffffffffffffffffffffffffff", "")
			result, err := observeCommand(t.Context(), pending, 2*time.Second)
			if err != nil {
				t.Fatalf("external owner result: %+v: %v", result, err)
			}
			if got := string(result.Stdout); got != tc.want {
				t.Fatalf("command observed marker %q, want %q", got, tc.want)
			}
		})
	}
}

func TestControlOwnerEnvironmentOwnsItsCopy(t *testing.T) {
	environment := []string{ownerMarkerKey + "=" + ownerSuppliedValue}
	executable := finiteExecutable(t, "printf '%s' \"${"+ownerMarkerKey+"-unset}\"")
	command := exec.Command(executable)
	command.Dir = filepath.Dir(executable)
	command.Env = environment
	pending := startControlOwnerTest(t, command, "ffffffffffffffffffffffffffffffff", "")
	environment[0] = ownerMarkerKey + "=mutated-after-start"
	result, err := observeCommand(t.Context(), pending, 2*time.Second)
	if err != nil {
		t.Fatalf("external owner result: %+v: %v", result, err)
	}
	if got := string(result.Stdout); got != ownerSuppliedValue {
		t.Fatalf("command observed marker %q after caller mutation, want %q", got, ownerSuppliedValue)
	}
}
