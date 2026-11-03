package supervisor

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestInfrastructureLaunchContextPreservesNativeSizedEnvironments(t *testing.T) {
	// This valid native environment stays below both supported hosts' aggregate
	// exec bounds, but JSON escaping pushes even one role beyond a control record.
	value := strings.Repeat("\x01", 70_000)
	environment := []string{"FIRST=" + value, "SECOND=" + value, "THIRD=" + value}
	command := exec.Command("/usr/bin/true")
	command.Env = slices.Clone(environment)
	// Run owns this finite fixture's exact Start and Wait. It launches no child.
	if err := command.Run(); err != nil {
		t.Fatalf("native-size fixture could not execute its original environment: %v", err)
	}
	store := newGenerationStore(t)
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	record, err := captureInfrastructureLaunchContext(pin, pueue.Options{
		Environment: environment, PrivateMaintenanceEnvironment: slices.Clone(environment),
	})
	if err != nil {
		t.Fatalf("native-valid snapshot refused by infrastructure capture: %v", err)
	}
	expected := expectedInfrastructureEnvironment(t, pin.Root(), pin.RootID(), environment)
	if !slices.Equal(record.ControlEnvironment, expected) || !slices.Equal(record.MaintenanceEnvironment, expected) {
		t.Fatal("native-sized selected environments changed during capture")
	}
}
