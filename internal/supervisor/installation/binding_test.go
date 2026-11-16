package installation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestBindingObservationComparesClientAndDaemonIndependently(t *testing.T) {
	for _, role := range []string{"current", "client", "daemon"} {
		t.Run(role, func(t *testing.T) {
			client, daemon := installedPairFixture(t, "selected client", "selected daemon")
			otherClient, otherDaemon := installedPairFixture(t, "recorded client", "recorded daemon")
			recorded := task.SupervisorRef{ClientExecutable: client, DaemonExecutable: daemon}
			if role == "client" {
				recorded.ClientExecutable = otherClient
			}
			if role == "daemon" {
				recorded.DaemonExecutable = otherDaemon
			}
			selected := ObserveSelectedSupervisorPair(ExecutableLookup{InitialExecutable: client})
			got := ObserveSupervisorBinding(BindingRuntimeSelected, selected, &recorded, nil)
			want := BindingDifferent
			if role == "current" {
				want = BindingCurrent
			}
			if got.Relation != want || got.Client.SelectedAvailability != AvailabilityPresent || got.Daemon.SelectedAvailability != AvailabilityPresent || got.Verification != "filesystem_metadata" {
				t.Fatalf("binding=%+v", got)
			}
			if role == "daemon" && (got.Client.Relation != BindingCurrent || got.Daemon.Relation != BindingDifferent) {
				t.Fatalf("daemon mismatch hidden behind matching client: %+v", got)
			}
		})
	}
}

func TestBindingMissingRoleRetainsIndependentAvailability(t *testing.T) {
	for _, missing := range []string{"pueue", "pueued"} {
		t.Run(missing, func(t *testing.T) {
			client, daemon := installedPairFixture(t, "selected client", "selected daemon")
			oldClient, oldDaemon := installedPairFixture(t, "recorded client", "recorded daemon")
			recorded := task.SupervisorRef{ClientExecutable: oldClient, DaemonExecutable: oldDaemon}
			if err := os.Remove(filepath.Join(filepath.Dir(client), missing)); err != nil {
				t.Fatal(err)
			}
			selected := ObserveSelectedSupervisorPair(ExecutableLookup{InitialExecutable: client})
			got := ObserveSupervisorBinding(BindingRuntimeSelected, selected, &recorded, nil)
			clientWant, daemonWant := AvailabilityPresent, AvailabilityMissing
			if missing == "pueue" {
				clientWant, daemonWant = AvailabilityMissing, AvailabilityPresent
			}
			if got.Relation != BindingUnknown || got.Client.SelectedAvailability != clientWant || got.Daemon.SelectedAvailability != daemonWant || got.HandoffAdvised() {
				t.Fatalf("partial selected pair fallback/mixed roles: %+v (daemon=%s)", got, daemon)
			}
		})
	}
}

func TestBindingMissingRecordedInstallationDoesNotProveDeath(t *testing.T) {
	client, _ := installedPairFixture(t, "selected client", "selected daemon")
	oldClient, oldDaemon := installedPairFixture(t, "recorded client", "recorded daemon")
	recorded := task.SupervisorRef{ClientExecutable: oldClient, DaemonExecutable: oldDaemon}
	if err := os.RemoveAll(filepath.Dir(oldClient)); err != nil {
		t.Fatal(err)
	}
	got := ObserveSupervisorBinding(BindingRuntimeSelected, ObserveSelectedSupervisorPair(ExecutableLookup{InitialExecutable: client}), &recorded, nil)
	if got.Relation != BindingUnknown || got.Client.RecordedAvailability != AvailabilityMissing || got.Daemon.RecordedAvailability != AvailabilityMissing || !got.HandoffAdvised() {
		t.Fatalf("missing publication metadata=%+v", got)
	}
}

func TestBindingCanonicalAliasesAndUnknownObservation(t *testing.T) {
	client, daemon := installedPairFixture(t, "selected client", "selected daemon")
	alias := filepath.Join(t.TempDir(), "client-alias")
	if err := os.Symlink(client, alias); err != nil {
		t.Fatal(err)
	}
	recorded := task.SupervisorRef{ClientExecutable: alias, DaemonExecutable: daemon}
	selected := ObserveSelectedSupervisorPair(ExecutableLookup{InitialExecutable: client})
	got := ObserveSupervisorBinding(BindingLoadedBundle, selected, &recorded, nil)
	if got.Relation != BindingCurrent || got.Scope != BindingLoadedBundle {
		t.Fatalf("canonical alias differs: %+v", got)
	}
	failed := ObserveSupervisorBinding(BindingLoadedBundle, selected, &recorded, os.ErrPermission)
	if failed.Relation != BindingUnknown || failed.Client.RecordedAvailability != AvailabilityUnknown || failed.HandoffAdvised() {
		t.Fatalf("unreadable publication inferred from path: %+v", failed)
	}
	absent := ObserveSupervisorBinding(BindingLoadedBundle, selected, nil, nil)
	if absent.Relation != BindingNotInitialized || absent.Client.SelectedAvailability != AvailabilityPresent {
		t.Fatalf("absence lost role facts: %+v", absent)
	}
}

func TestBindingAvailabilityDoesNotHideJoinedPermissionFault(t *testing.T) {
	err := errors.Join(pueue.ErrConfiguration, os.ErrNotExist, os.ErrPermission)
	if got := roleAvailability(BundleRole{Path: "/selected/role", Err: err}); got != AvailabilityUnknown {
		t.Fatalf("joined fault became absence: %s", got)
	}
}
