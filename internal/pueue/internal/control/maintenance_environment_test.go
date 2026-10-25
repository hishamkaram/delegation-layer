package control

import (
	"context"
	"slices"
	"testing"
)

func TestMaintenanceHandoffRetainsSelectedEnvironment(t *testing.T) {
	t.Setenv("LANG", "issue91-caller")
	for _, test := range []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "default", want: "issue91-caller"},
		{name: "empty", environment: []string{}},
		{name: "selected", environment: []string{"LANG=issue91-selected"}, want: "issue91-selected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			owned := slices.Clone(test.environment)
			received := handOffMaintenance(t, Options{PrivateMaintenanceEnvironment: owned})
			if received == nil || environmentValue(received, "LANG") != test.want {
				t.Fatal("maintenance handoff did not retain the selected environment")
			}
			if !slices.Equal(owned, test.environment) {
				t.Fatal("maintenance callback changed the owner's environment")
			}
		})
	}
}

func TestMaintenanceHandoffIgnoresControlEnvironment(t *testing.T) {
	t.Setenv("LANG", "issue91-caller")
	received := handOffMaintenance(t, Options{Environment: []string{"LANG=issue91-control"}})
	if got := environmentValue(received, "LANG"); got != "issue91-caller" {
		t.Fatalf("maintenance handoff used the control environment: LANG=%q", got)
	}
}

func TestNormalizeOptionsOwnsMaintenanceEnvironment(t *testing.T) {
	selected := []string{"LANG=issue91-selected"}
	normalized, err := normalizeOptions(Options{PrivateMaintenanceEnvironment: selected})
	if err != nil {
		t.Fatal(err)
	}
	selected[0] = "LANG=mutated-after-normalize"
	if got := environmentValue(normalized.PrivateMaintenanceEnvironment, "LANG"); got != "issue91-selected" {
		t.Fatalf("normalized maintenance environment aliases caller input: LANG=%q", got)
	}
	empty, err := normalizeOptions(Options{PrivateMaintenanceEnvironment: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if empty.PrivateMaintenanceEnvironment == nil || len(empty.PrivateMaintenanceEnvironment) != 0 {
		t.Fatal("explicit empty maintenance environment changed meaning")
	}
	unset, err := normalizeOptions(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if unset.PrivateMaintenanceEnvironment != nil {
		t.Fatal("absent maintenance environment became an explicit selection")
	}
}

// handOffMaintenance persists one maintenance wake, schedules it through the
// production handoff, and returns the environment the launcher received. The
// launcher mutates its copy to prove the handoff does not share owner state.
func handOffMaintenance(t *testing.T, options Options) []string {
	t.Helper()
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	client.options.Environment = options.Environment
	client.options.PrivateMaintenanceEnvironment = options.PrivateMaintenanceEnvironment
	calls := 0
	var received []string
	client.options.PrivateMaintenanceLauncher = func(_ context.Context, _, _ string, environment []string) error {
		calls++
		received = slices.Clone(environment)
		if len(environment) != 0 {
			environment[0] = "LANG=callback-mutated"
		}
		return nil
	}
	if err := RequestPrivateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if !client.ScheduleRequestedPrivateQueueMaintenance(t.Context()) || calls != 1 {
		t.Fatal("persisted maintenance wake was not handed off once")
	}
	return received
}
