package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateAdmissionAdoptsReadyLegacyOwnerBeforeScheduling(t *testing.T) {
	store, deps, legacy := newSupervisorProbeTestFixture(t)
	identity := removeLegacyAdoptionTestBinding(t, store.Root, legacy.Binding())
	base := filepath.Join(store.Root, ".supervisor")
	starts := readSupervisorProbeTestFile(t, filepath.Join(base, "daemon-started"))
	control, cancelControl := supervisorcore.ControlContextWithParent(t.Context(), deps.SupervisorOptions)
	defer cancelControl()
	client, err := supervisorcore.PreparePrivateAdmissionSupervisorContext(control, deps.supervisorLookup(), store, deps.SupervisorOptions)
	if client != nil {
		t.Cleanup(func() {
			if closeErr := client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil || client == nil {
		t.Fatalf("legacy ready owner could not reach scheduling: client=%v err=%v", client, err)
	}
	assertLegacyAdoptionTestOwner(t, store.Root, legacy.Binding(), identity)
	assertSupervisorProbeTestFile(t, filepath.Join(base, "daemon-started"), starts)
	configured, err := supervisorcore.OrdinarySchedulingRecorded(store.Root, client.Binding())
	if err != nil || !configured {
		t.Errorf("adopted legacy owner did not publish scheduling: configured=%t err=%v", configured, err)
	}
}

func TestStatusAdoptsExactReadyLegacyOwnerBeforeObservation(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, 7)
	identity := removeLegacyAdoptionTestBinding(t, fixture.root, fixture.predecessor)
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	code, stdout, stderr, response := runHistoricalSupervisorCommand(t, fixture, []string{
		"--root", fixture.root, "status", fixture.req.TaskID, "--json",
	})
	if code != 0 || response.Admission != task.AdmissionAdmitted.String() || !historicalSupervisorResponseMatches(response.Supervisor, 7) {
		t.Errorf("legacy ready task was not observed: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	assertLegacyAdoptionTestOwner(t, fixture.root, fixture.predecessor, identity)
	assertSupervisorProbeTestFile(t, fixture.daemonStartLog, starts)
	assertHistoricalTaskEvidence(t, fixture)
}

func removeLegacyAdoptionTestBinding(t *testing.T, root string, binding task.SupervisorRef) []byte {
	t.Helper()
	identity, err := task.MarshalCanonical(historicalPrivateDaemonIdentity{Executable: binding.DaemonExecutable, SHA256: binding.DaemonSHA256})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, ".supervisor")
	if err = os.WriteFile(filepath.Join(base, "daemon.identity.json"), identity, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(base, "supervisor.identity.json")); err != nil {
		t.Fatal(err)
	}
	return identity
}

func assertLegacyAdoptionTestOwner(t *testing.T, root string, want task.SupervisorRef, identity []byte) {
	t.Helper()
	published, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil || !found || published != want {
		t.Errorf("legacy owner not published exactly: found=%t binding=%+v err=%v", found, published, err)
	}
	after := readSupervisorProbeTestFile(t, filepath.Join(root, ".supervisor", "daemon.identity.json"))
	if !bytes.Equal(identity, after) {
		t.Error("legacy adoption replaced the daemon launch identity")
	}
}

// TestQueueMaintenanceAdoptsLegacyOwnerOnlyWhenProven: a maintenance wake on a
// historical root without the logical binding adopts the exact ready daemon
// through the canonical resolver and binding owner without starting it. An
// unavailable daemon or a foreign daemon identity is refused without a start
// and the durable wake stays pending.
func TestQueueMaintenanceAdoptsLegacyOwnerOnlyWhenProven(t *testing.T) {
	for _, scenario := range []string{"ready", "unavailable", "foreign"} {
		t.Run(scenario, func(t *testing.T) {
			store, deps, legacy := newSupervisorProbeTestFixture(t)
			identity := prepareLegacyMaintenanceScenario(t, store.Root, legacy.Binding(), scenario)
			base := filepath.Join(store.Root, ".supervisor")
			if err := pueue.RequestPrivateQueueMaintenance(t.Context(), store.Root); err != nil {
				t.Fatal(err)
			}
			starts := readSupervisorProbeTestFile(t, filepath.Join(base, "daemon-started"))
			err := supervisorcore.RunQueueMaintenance(store.Root, deps.SupervisorOptions, deps.supervisorLookup())
			assertSupervisorProbeTestFile(t, filepath.Join(base, "daemon-started"), starts)
			if scenario != "ready" {
				assertLegacyMaintenanceRefused(t, store.Root, identity, err)
				return
			}
			if errors.Is(err, pueue.ErrBinding) {
				t.Fatalf("ready legacy owner was refused: %v", err)
			}
			assertLegacyAdoptionTestOwner(t, store.Root, legacy.Binding(), identity)
		})
	}
}

// prepareLegacyMaintenanceScenario removes the logical binding and returns the
// daemon launch identity the scenario leaves recorded.
func prepareLegacyMaintenanceScenario(t *testing.T, root string, binding task.SupervisorRef, scenario string) []byte {
	t.Helper()
	identity := removeLegacyAdoptionTestBinding(t, root, binding)
	base := filepath.Join(root, ".supervisor")
	switch scenario {
	case "unavailable":
		if err := os.Remove(filepath.Join(base, "ready")); err != nil {
			t.Fatal(err)
		}
	case "foreign":
		foreign, err := task.MarshalCanonical(historicalPrivateDaemonIdentity{Executable: binding.DaemonExecutable, SHA256: strings.Repeat("0", 64)})
		if err != nil {
			t.Fatal(err)
		}
		writeAppTestFile(t, filepath.Join(base, "daemon.identity.json"), foreign)
		return foreign
	}
	return identity
}

func assertLegacyMaintenanceRefused(t *testing.T, root string, identity []byte, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("an unproven legacy owner was adopted")
	}
	if _, found, readErr := pueue.ReadPrivateSupervisorBinding(root); readErr != nil || found {
		t.Fatalf("refused adoption published a binding: found=%t err=%v", found, readErr)
	}
	if pending, pendingErr := pueue.PrivateQueueMaintenanceRetryNeeded(root); pendingErr != nil || !pending {
		t.Fatalf("refused adoption consumed the wake: pending=%t err=%v", pending, pendingErr)
	}
	if after := readSupervisorProbeTestFile(t, filepath.Join(root, ".supervisor", "daemon.identity.json")); !bytes.Equal(after, identity) {
		t.Fatal("refused adoption replaced the daemon launch identity")
	}
}
