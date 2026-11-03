package supervisor

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestInfrastructureLaunchContextPreservesSelectedCallbackAndMaintenanceScopes(t *testing.T) {
	store := newGenerationStore(t)
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	base := OptionsForEnvironment(pueue.Options{}, []string{"HOME=/caller", "PATH=/caller/bin"})
	options := OptionsWithEnvironment(base, []string{"HOME=/provider", "CALLBACK_CONTEXT=preserved", "XDG_CONFIG_HOME=/provider-config"})
	record, err := captureInfrastructureLaunchContext(pin, options)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(record.ControlEnvironment, expectedInfrastructureEnvironment(t, pin.Root(), pin.RootID(), []string{"HOME=/provider", "PATH=/caller/bin", "CALLBACK_CONTEXT=preserved"})) ||
		!slices.Equal(record.MaintenanceEnvironment, expectedInfrastructureEnvironment(t, pin.Root(), pin.RootID(), []string{"HOME=/caller", "PATH=/caller/bin"})) {
		t.Fatal("capture changed the effective callback or maintenance scope")
	}
	options.Environment[0] = "HOME=/changed"
	options.PrivateMaintenanceEnvironment[0] = "HOME=/changed"
	if record.ControlEnvironment[0] != "HOME=/provider" || record.MaintenanceEnvironment[0] != "HOME=/caller" {
		t.Fatal("capture retained caller-owned environment slices")
	}
}

func TestInfrastructureLaunchContextDistinguishesDefaultAndExplicitEmpty(t *testing.T) {
	t.Setenv("HOME", "/ambient-control")
	t.Setenv("UNRELATED_PRIVATE_VALUE", "must-not-be-captured")
	store := newGenerationStore(t)
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defaulted, err := captureInfrastructureLaunchContext(pin, pueue.Options{})
	if err != nil || !slices.Contains(defaulted.ControlEnvironment, "HOME=/ambient-control") ||
		slices.Contains(defaulted.ControlEnvironment, "UNRELATED_PRIVATE_VALUE=must-not-be-captured") {
		t.Fatalf("capture did not reuse the existing default policy: %v", err)
	}
	explicit, err := captureInfrastructureLaunchContext(pin, pueue.Options{Environment: []string{}, PrivateMaintenanceEnvironment: []string{}})
	if err != nil || !slices.Equal(explicit.ControlEnvironment, expectedInfrastructureEnvironment(t, pin.Root(), pin.RootID(), nil)) ||
		!slices.Equal(explicit.MaintenanceEnvironment, expectedInfrastructureEnvironment(t, pin.Root(), pin.RootID(), nil)) {
		t.Fatalf("capture replaced explicit empty environments: %v", err)
	}
}

func TestInfrastructureLaunchContextRebasesReservedOverlay(t *testing.T) {
	store := newGenerationStore(t)
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	selected := []string{"PROVIDER=" + string([]byte{0xff}), "DELEGATION_LAYER_PROCESS_SCOPE=old", "DELEGATION_LAYER_PROCESS_SCOPE=duplicate", "DELEGATION_LAYER_PROCESS_SCOPE_NEAR=keep", infrastructure.OriginEnvironment + "=old-A", infrastructure.OriginEnvironment + "=duplicate-A"}
	before := slices.Clone(selected)
	record, err := captureInfrastructureLaunchContext(pin, pueue.Options{Environment: selected, PrivateMaintenanceEnvironment: selected})
	if err != nil {
		t.Fatalf("reserved overlay was not rebased: %v", err)
	}
	for _, environment := range [][]string{record.ControlEnvironment, record.MaintenanceEnvironment} {
		if len(environment) != 3 || environment[0] != before[0] || environment[1] != before[3] {
			t.Fatal("overlay changed provider bytes or retained finite scope")
		}
		origin, found, readErr := infrastructure.ReadOrigin(environment)
		if readErr != nil || !found || origin.Root() != pin.Root() || origin.RootID() != pin.RootID() {
			t.Fatalf("overlay did not select B origin: %v", readErr)
		}
	}
	record.ControlEnvironment[0] = "changed"
	if !slices.Equal(selected, before) || record.MaintenanceEnvironment[0] != before[0] {
		t.Fatal("overlay aliases caller or other role")
	}
}

func expectedInfrastructureEnvironment(t *testing.T, root, rootID string, environment []string) []string {
	t.Helper()
	origin, err := infrastructure.NewOrigin(root, rootID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := infrastructure.WithOrigin(environment, origin)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
