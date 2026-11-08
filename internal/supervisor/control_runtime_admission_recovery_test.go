package supervisor

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These production-store consumers prove admission selection and its durable
// gate. The finite clients and owned socket do not model a native daemon.
func TestControlRuntimeAdmissionRecoversInstalledTransport(t *testing.T) {
	for _, damaged := range []string{"client", "daemon", "both", "historical-copy"} {
		t.Run(damaged, func(t *testing.T) {
			testControlRuntimeAdmissionInstalledTransport(t, damaged)
		})
	}
}

func testControlRuntimeAdmissionInstalledTransport(t *testing.T, damaged string) {
	t.Helper()
	fixture := newAdmissionRecoveryFixture(t, true)
	transport := parentMigrationPair(t, fixture, "admission-transport")
	prepareAdmissionRecoveryDamage(t, &fixture, damaged)
	before := parentMigrationTree(t, fixture.store.Root)
	calls := 0
	runtime := ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: transport.ClientExecutable}, Routing: admissionRecoveryNoRoute(&calls)}
	client, err := runtime.PreparePrivateAdmission(t.Context(), fixture.store)
	want := fixture.target
	if damaged == "client" || damaged == "both" || damaged == "historical-copy" {
		want.ClientExecutable = transport.ClientExecutable
	}
	if damaged != "client" {
		want.DaemonExecutable = transport.DaemonExecutable
	}
	requireCompatibleControlClient(t, client, err, fixture.target, want)
	if calls != 0 {
		t.Fatalf("passive recovery contacted owner %d times", calls)
	}
	if _, err := os.Stat(filepath.Join(fixture.store.Root, "historical-copy-executed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical copied executable ran: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("passive admission changed generation, scheduling or historical-copy state")
	}
}

func TestControlRuntimeAdmissionHealthyPairIgnoresInvalidSelection(t *testing.T) {
	fixture := newAdmissionRecoveryFixture(t, true)
	lookup := installation.ExecutableLookup{AcceptedBootstrapPair: installation.BootstrapPair{Client: "invalid relative installation"}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup}).PreparePrivateAdmission(t.Context(), fixture.store)
	requireCompatibleControlClient(t, client, err, fixture.target, fixture.target)
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("healthy admission consulted or staged another installation")
	}
}

func TestControlRuntimeAdmissionUnavailableSelectionKeepsMigrationGuidance(t *testing.T) {
	fixture := newAdmissionRecoveryFixture(t, true)
	prepareAdmissionRecoveryDamage(t, &fixture, "client")
	lookup := installation.ExecutableLookup{InitialExecutable: filepath.Join(fixture.store.Root, "missing-installation", "pueue")}
	calls := 0
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup, Routing: admissionRecoveryNoRoute(&calls)}).PreparePrivateAdmission(t.Context(), fixture.store)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("unavailable installation returned admission")))
	}
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, pueue.ErrBinding) || calls != 0 {
		t.Fatalf("optional installation absence changed refusal: routes=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("unavailable transport changed durable state")
	}
}

func TestControlRuntimeAdmissionSelectionFaultIsNotOptionalAbsence(t *testing.T) {
	fixture := newAdmissionRecoveryFixture(t, true)
	prepareAdmissionRecoveryDamage(t, &fixture, "client")
	directory := supervisorfixture.CanonicalTempDir(t)
	nonExecutable := filepath.Join(directory, "pueued")
	if err := os.WriteFile(nonExecutable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := installation.ExecutableLookup{AcceptedBootstrapPair: installation.BootstrapPair{Client: filepath.Join(directory, "missing-pueue"), Daemon: nonExecutable}}
	calls := 0
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup, Routing: admissionRecoveryNoRoute(&calls)}).PreparePrivateAdmission(t.Context(), fixture.store)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("invalid installation returned admission")))
	}
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, pueue.ErrConfiguration) || !strings.Contains(err.Error(), "must be executable regular file") || errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || calls != 0 {
		t.Fatalf("joined installation fault was hidden as optional absence: routes=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("failed installation selection changed durable state")
	}
}

func TestControlRuntimeAdmissionPreliminaryFaultUsesCanonicalProbe(t *testing.T) {
	for _, fault := range []string{"empty-client", "empty-daemon", "stat-loop", "recorded-daemon-alias"} {
		t.Run(fault, func(t *testing.T) {
			testControlRuntimeAdmissionPreliminaryFault(t, fault)
		})
	}
}

func testControlRuntimeAdmissionPreliminaryFault(t *testing.T, fault string) {
	t.Helper()
	fixture := newAdmissionRecoveryFixture(t, true)
	prepareAdmissionRecoveryDamage(t, &fixture, fault)
	_, originalErr := ProbePrivateAdmissionSupervisor(t.Context(), fixture.store, "", "", fixture.options)
	if originalErr == nil {
		t.Fatal("fault fixture unexpectedly satisfies canonical admission")
	}
	calls := 0
	before := parentMigrationTree(t, fixture.store.Root)
	lookup := installation.ExecutableLookup{AcceptedBootstrapPair: installation.BootstrapPair{Client: "invalid relative installation"}}
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup, Routing: admissionRecoveryNoRoute(&calls)}).PreparePrivateAdmission(t.Context(), fixture.store)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("malformed publication returned admission")))
	}
	if err == nil || err.Error() != originalErr.Error() || calls != 0 {
		t.Fatalf("preliminary observation replaced canonical fault: routes=%d err=%v want=%v", calls, err, originalErr)
	}
	if fault == "stat-loop" && (!errors.Is(err, syscall.ELOOP) || !errors.Is(err, pueue.ErrBinding) || !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired)) {
		t.Fatalf("stat fault lost wrapped recovery guidance: %v", err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("preliminary fault changed durable state")
	}
}

func TestControlRuntimeAdmissionUnreadyTransportKeepsOriginalRefusal(t *testing.T) {
	for _, state := range []string{"stopped-endpoint", "missing-observation-lock", "stale-scheduling"} {
		t.Run(state, func(t *testing.T) {
			fixture := newAdmissionRecoveryFixture(t, state != "stopped-endpoint")
			transport := parentMigrationPair(t, fixture, "admission-transport")
			prepareAdmissionRecoveryDamage(t, &fixture, "client")
			switch state {
			case "missing-observation-lock":
				removeCompatibleControlExecutable(t, filepath.Join(fixture.store.Root, ".supervisor", "task-observation.lock"))
			case "stale-scheduling":
				writeAdmissionRecoveryScheduling(t, fixture, fixture.legacy)
			}
			calls := 0
			before := parentMigrationTree(t, fixture.store.Root)
			client, err := (ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: transport.ClientExecutable}, Routing: admissionRecoveryNoRoute(&calls)}).PreparePrivateAdmission(t.Context(), fixture.store)
			if client != nil {
				t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("unready transport returned admission")))
			}
			if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || calls != 0 {
				t.Fatalf("unready fallback changed canonical refusal into routing: routes=%d err=%v", calls, err)
			}
			if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
				t.Fatal("unready fallback changed generation or scheduling evidence")
			}
		})
	}
}

func newAdmissionRecoveryFixture(t *testing.T, live bool) parentMigrationFixture {
	t.Helper()
	fixture, _ := newWarmContinuationFixture(t)
	base := filepath.Dir(fixture.target.ConfigPath)
	if err := os.WriteFile(filepath.Join(base, "task-observation.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeAdmissionRecoveryScheduling(t, fixture, fixture.target)
	if !live {
		return fixture
	}
	listener, err := net.Listen("unix", strings.TrimPrefix(fixture.target.Endpoint, "unix:"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return fixture
}

func writeAdmissionRecoveryScheduling(t *testing.T, fixture parentMigrationFixture, binding task.SupervisorRef) {
	t.Helper()
	writeContinuationFixtureRecord(t, filepath.Join(fixture.store.Root, ".supervisor", "ordinary-scheduling.json"), privateOrdinarySchedulingRecord{
		SchemaVersion: task.SchemaVersion, Revision: privateOrdinarySchedulingRevision, Supervisor: binding,
		StagedLaunch: pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true},
	})
}

func prepareAdmissionRecoveryDamage(t *testing.T, fixture *parentMigrationFixture, damaged string) {
	t.Helper()
	switch damaged {
	case "client", "daemon", "both":
		if damaged != "daemon" {
			removeCompatibleControlExecutable(t, fixture.target.ClientExecutable)
		}
		if damaged != "client" {
			removeCompatibleControlExecutable(t, fixture.target.DaemonExecutable)
		}
	case "empty-client":
		fixture.target.ClientExecutable = ""
	case "empty-daemon":
		fixture.target.DaemonExecutable = ""
	case "stat-loop":
		removeCompatibleControlExecutable(t, fixture.target.ClientExecutable)
		if err := os.Symlink(fixture.target.ClientExecutable, fixture.target.ClientExecutable); err != nil {
			t.Fatal(err)
		}
	case "recorded-daemon-alias":
		transport := parentMigrationPair(t, *fixture, "daemon-alias")
		removeCompatibleControlExecutable(t, fixture.target.DaemonExecutable)
		if err := os.Symlink(transport.DaemonExecutable, fixture.target.DaemonExecutable); err != nil {
			t.Fatal(err)
		}
	case "historical-copy":
		directory := filepath.Join(fixture.store.Root, ".supervisor", "pueue-pair-"+strings.Repeat("a", 64))
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		fixture.target.ExecutableIdentity = ""
		fixture.target.ClientSHA256, fixture.target.DaemonSHA256 = strings.Repeat("b", 64), strings.Repeat("c", 64)
		fixture.target.ClientExecutable, fixture.target.DaemonExecutable = filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
		fixture.options.Environment = append(fixture.options.Environment, "HISTORICAL_POISON_MARKER="+filepath.Join(fixture.store.Root, "historical-copy-executed"))
		poison := "#!/bin/sh\n: >\"${HISTORICAL_POISON_MARKER:?missing owned poison marker}\"\nexit 99\n"
		supervisorfixture.WriteExecutable(t, fixture.target.ClientExecutable, poison)
		supervisorfixture.WriteExecutable(t, fixture.target.DaemonExecutable, poison)
	default:
		t.Fatalf("unknown admission fixture damage %q", damaged)
	}
	if damaged == "historical-copy" || damaged == "empty-client" || damaged == "empty-daemon" {
		writeContinuationGeneration(t, *fixture, nil)
		writeAdmissionRecoveryScheduling(t, *fixture, fixture.target)
	}
}

func admissionRecoveryNoRoute(calls *int) controlRoutingFunc {
	return func(context.Context, *taskdir.Store) (infrastructure.Disposition, error) {
		*calls++
		return infrastructure.Disposition{}, errors.New("live admission must not route a preparation")
	}
}
