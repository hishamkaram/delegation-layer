package supervisor

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
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
	if !slices.Equal(record.ControlEnvironment, []string{"HOME=/provider", "PATH=/caller/bin", "CALLBACK_CONTEXT=preserved"}) ||
		!slices.Equal(record.MaintenanceEnvironment, []string{"HOME=/caller", "PATH=/caller/bin"}) {
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
	if err != nil || explicit.ControlEnvironment == nil || len(explicit.ControlEnvironment) != 0 ||
		explicit.MaintenanceEnvironment == nil || len(explicit.MaintenanceEnvironment) != 0 {
		t.Fatalf("capture replaced explicit empty environments: %v", err)
	}
}
