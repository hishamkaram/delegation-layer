package app

import (
	"bytes"
	"os"
	"path/filepath"
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
	client, err := supervisorcore.PreparePrivateAdmissionSupervisor(deps.InitialSupervisorExecutable, store, deps.SupervisorOptions)
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
