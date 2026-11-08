package supervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These canonical store consumers use finite command fixtures. They establish
// passive generation/transport selection, not native daemon placement or exit.
func TestControlContinuationReadsCompatibleRuntimeWithoutInventory(t *testing.T) {
	for _, damaged := range []string{"client", "daemon", "both"} {
		t.Run(damaged, func(t *testing.T) {
			fixture, source := newWarmContinuationFixture(t)
			transport := compatibleControlTransport(t, fixture, damaged)
			before := parentMigrationTree(t, fixture.store.Root)
			client, err := (ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: transport.ClientExecutable}}).PrepareContinuation(t.Context(), fixture.store, source)
			requireCompatibleControlClient(t, client, err, fixture.target, transport)
			if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
				t.Fatal("passive continuation changed durable state or staged inventory")
			}
		})
	}
}

func TestControlRuntimeSavedCapturedPATHRefusalDoesNotRouteOwner(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	removeCompatibleControlExecutable(t, fixture.target.ClientExecutable)
	removeCompatibleControlExecutable(t, fixture.legacy.ClientExecutable)
	available := supervisorfixture.CanonicalTempDir(t)
	supervisorfixture.WriteExecutable(t, filepath.Join(available, "pueue"), parentMigrationClient)
	supervisorfixture.WriteExecutable(t, filepath.Join(available, "pueued"), "#!/bin/sh\nexit 0\n")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, available)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	routing := selectedControlRouting{saved: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		return infrastructure.Disposition{}, errors.New("owner must not receive a refused lookup")
	}}
	lookup := installation.ExecutableLookup{Environment: []string{"PATH=" + relative}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup, Routing: routing}).RecoverSaved(t.Context(), fixture.store, fixture.legacy, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("refused lookup returned a client")))
	}
	if !errors.Is(err, exec.ErrDot) || calls != 0 {
		t.Fatalf("lookup refusal softened into owner recovery: calls=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("refused lookup changed durable state")
	}
}

func TestPrepareAdmissionSupervisorAcceptsCompatibleDaemonBeforeScheduling(t *testing.T) {
	fixture, _ := newWarmContinuationFixture(t)
	source := routedMetadataSourceForTask(t, fixture, fixture.target, strings.Repeat("b", 32))
	transport := compatibleControlTransport(t, fixture, "client")
	runtimeBinding := fixture.target
	runtimeBinding.ClientExecutable, runtimeBinding.DaemonExecutable = transport.ClientExecutable, transport.DaemonExecutable
	writeContinuationFixtureRecord(t, filepath.Join(fixture.store.Root, ".supervisor", "ordinary-scheduling.json"), privateOrdinarySchedulingRecord{
		SchemaVersion: task.SchemaVersion, Revision: privateOrdinarySchedulingRevision, Supervisor: runtimeBinding,
		StagedLaunch: pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true},
	})
	selection := AdmissionSelection{SavedSupervisor: &fixture.target, ContinuationSource: source}
	runtime := AdmissionOptions{Control: fixture.options, InitialExecutable: transport.ClientExecutable}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := PrepareAdmissionSupervisorContext(t.Context(), selection, runtime, fixture.store, false, "", true)
	requireCompatibleControlClient(t, client, err, fixture.target, runtimeBinding)
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("compatible transport changed canonical generation, scheduling receipt, task records or queue-repair state")
	}
}

func TestPrivateAdmissionRuntimeRefusesDifferentLogicalGenerationWithoutAuthorization(t *testing.T) {
	fixture, _ := newWarmContinuationFixture(t)
	selected := fixture.target
	selected.ConfigDigest = task.ComputeSHA256([]byte("different bound configuration"))
	before := parentMigrationTree(t, fixture.store.Root)
	err := pueue.CheckPrivateAdmissionRuntime(t.Context(), fixture.store.Root, selected)
	if !errors.Is(err, pueue.ErrPrivateSupervisorSubmissionIncomplete) {
		t.Fatalf("different logical generation gained admission: %v", err)
	}
	if _, present, readErr := pueue.ReadPrivateSchedulingEvidence(fixture.store.Root); readErr != nil || present {
		t.Fatalf("refused generation published scheduling authority: present=%t err=%v", present, readErr)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("early generation refusal changed canonical records or queue-repair state")
	}
}

func TestPrepareTaskSupervisorDefersCompatibleRuntimeMaintenance(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	transport := compatibleControlTransport(t, fixture, "client")
	td, err := fixture.store.OpenTask(source.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	_, meta, readErr := td.PreparedRecords()
	if err = errors.Join(readErr, td.Close()); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, runner, "new managed runner fixture")
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := PrepareTaskSupervisorWithRouting(t.Context(), fixture.store, fixture.legacy, meta, TaskSupervisorObservationOptions{
		Control: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: transport.ClientExecutable}, RunnerExecutable: runner, Source: source,
	})
	if client != nil {
		defer func() {
			if closeErr := client.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("compatible observation installed a runner or published a maintenance receipt")
	}
	if err != nil || client != nil {
		t.Fatalf("compatible observation did not defer automatic maintenance: client=%v err=%v", client != nil, err)
	}
}

func TestControlRuntimeSavedUnavailablePairStillRoutesOwner(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	removeCompatibleControlExecutable(t, fixture.target.ClientExecutable)
	removeCompatibleControlExecutable(t, fixture.legacy.ClientExecutable)
	want := errors.New("owner route result")
	calls := 0
	routing := selectedControlRouting{saved: func(_ context.Context, store *taskdir.Store, _ infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if store != fixture.store || observed != source {
			t.Fatal("unavailable transport changed owner routing target")
		}
		return infrastructure.Disposition{}, want
	}}
	transport := capturedPATHControlPair(t, fixture.target)
	lookup := installation.ExecutableLookup{InitialExecutable: transport.ClientExecutable}
	fixture.options.Environment = append(fixture.options.Environment, "CONTROL_FIXTURE_UNAVAILABLE=1")
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup, Routing: routing}).RecoverSaved(t.Context(), fixture.store, fixture.legacy, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("failed owner contact returned a client")))
	}
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("unavailable pair blocked or repeated owner route: calls=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("unavailable pair performed local preparation")
	}
}

func TestControlRuntimeSavedEmptyCapturedPATHRefusesOwnerRoute(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	removeCompatibleControlExecutable(t, fixture.target.ClientExecutable)
	removeCompatibleControlExecutable(t, fixture.legacy.ClientExecutable)
	ambient := capturedPATHControlPair(t, fixture.target)
	t.Setenv("PATH", filepath.Dir(ambient.ClientExecutable))
	lookup := installation.ExecutableLookup{Environment: []string{"PATH=" + supervisorfixture.CanonicalTempDir(t)}}
	want := exec.ErrNotFound
	calls := 0
	routing := selectedControlRouting{saved: func(_ context.Context, store *taskdir.Store, _ infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if store != fixture.store || observed != source {
			t.Fatal("captured PATH absence changed the owner subject")
		}
		return infrastructure.Disposition{}, want
	}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup, Routing: routing}).RecoverSaved(t.Context(), fixture.store, fixture.legacy, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("empty captured PATH used an ambient client")))
	}
	if !errors.Is(err, want) || calls != 0 {
		t.Fatalf("captured PATH absence reached owner contact or ambient fallback: calls=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("captured PATH absence performed local preparation")
	}
}

func TestControlRuntimeSavedReadsCapturedPATHTransport(t *testing.T) {
	fixture, _ := newWarmContinuationFixture(t)
	source := routedMetadataSourceForTask(t, fixture, fixture.target, strings.Repeat("b", 32))
	transport := capturedPATHControlPair(t, fixture.target)
	for _, path := range []string{fixture.target.ClientExecutable, fixture.target.DaemonExecutable} {
		removeCompatibleControlExecutable(t, path)
	}
	t.Setenv("PATH", supervisorfixture.CanonicalTempDir(t))
	lookup := installation.ExecutableLookup{Environment: []string{"PATH=" + filepath.Dir(transport.ClientExecutable)}}
	calls := 0
	routing := selectedControlRouting{saved: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		return infrastructure.Disposition{}, errors.New("live captured PATH transport must not route preparation")
	}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup, Routing: routing}).RecoverSaved(t.Context(), fixture.store, fixture.target, source)
	requireCompatibleControlClient(t, client, err, fixture.target, transport)
	if calls != 0 {
		t.Fatalf("live captured PATH transport contacted owner %d times", calls)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("captured PATH transport changed durable state or staged inventory")
	}
}

func capturedPATHControlPair(t *testing.T, saved task.SupervisorRef) task.SupervisorRef {
	t.Helper()
	directory := supervisorfixture.CanonicalTempDir(t)
	client, daemon := parentMigrationClient+"\n# captured PATH transport\n", "#!/bin/sh\nexit 0\n# captured PATH daemon\n"
	saved.ClientExecutable, saved.DaemonExecutable = filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	saved.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	saved.ClientSHA256, saved.DaemonSHA256 = "", ""
	supervisorfixture.WriteExecutable(t, saved.ClientExecutable, client)
	supervisorfixture.WriteExecutable(t, saved.DaemonExecutable, daemon)
	return saved
}

func TestControlRuntimeSavedReadsCompatibleRuntimeWithoutInventory(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	transport := compatibleControlTransport(t, fixture, "client")
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: transport.ClientExecutable}}).RecoverSaved(t.Context(), fixture.store, fixture.legacy, source)
	// Ordinary saved recovery preserves MAIN's original saved-A candidate.
	// Its runtime config is active C; continuation instead supplies derived C.
	want := fixture.target
	want.ClientExecutable, want.ClientSHA256 = fixture.legacy.ClientExecutable, fixture.legacy.ClientSHA256
	want.DaemonExecutable, want.DaemonSHA256 = fixture.legacy.DaemonExecutable, fixture.legacy.DaemonSHA256
	requireCompatibleControlClient(t, client, err, fixture.legacy, want)
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("passive saved recovery changed durable state or staged inventory")
	}
}

func TestControlContinuationPreservesLegacyClientUpdateAndUnusedInventoryFault(t *testing.T) {
	fixture := newSavedControlFixture(t)
	fixture.target = legacyRawControlPair(t, fixture)
	fixture.legacy = fixture.target
	base := filepath.Join(fixture.store.Root, ".supervisor")
	for _, directory := range []string{"data", "run"} {
		if err := os.MkdirAll(filepath.Join(base, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeHistoricalSupervisorBinding(t, fixture.store.Root, fixture.target)
	writeContinuationFixtureRecord(t, filepath.Join(base, "daemon.identity.json"), parentMigrationIdentity(fixture.target))
	source := routedMetadataSource(t, fixture, fixture.target)
	changed := []byte(parentMigrationClient + "\n# supported updated legacy client\n")
	if err := os.WriteFile(fixture.target.ClientExecutable, changed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "pueue-pair.json"), []byte("invalid unused inventory"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := fixture.target
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: "/unavailable-installation/pueue"}}).PrepareContinuation(t.Context(), fixture.store, source)
	requireCompatibleControlClient(t, client, err, fixture.target, want)
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("legacy client update changed durable state during observation")
	}
}

func TestControlContinuationRefusesDamagedManagedPair(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	transport := compatibleControlTransport(t, fixture, "nonverifying")
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: installation.ExecutableLookup{InitialExecutable: transport.ClientExecutable}}).PrepareContinuation(t.Context(), fixture.store, source)
	if client != nil {
		t.Fatal(ClosePrivateAdmissionTransition(client, errors.New("damaged managed pair returned a client")))
	}
	if err == nil {
		t.Fatal("managed integrity fault was ignored in favor of another installation")
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("managed integrity refusal changed durable state")
	}
}

func TestControlContinuationHealthyPairIgnoresUnavailableTransport(t *testing.T) {
	fixture, source := newWarmContinuationFixture(t)
	lookup := installation.ExecutableLookup{InitialExecutable: filepath.Join(fixture.store.Root, "unavailable-installation", "pueue")}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Lookup: lookup}).PrepareContinuation(t.Context(), fixture.store, source)
	requireCompatibleControlClient(t, client, err, fixture.target, fixture.target)
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("healthy continuation changed durable state")
	}
}

func compatibleControlTransport(t *testing.T, fixture parentMigrationFixture, damaged string) task.SupervisorRef {
	t.Helper()
	transport := parentMigrationPair(t, fixture, "compatible-transport")
	// Model a supported legacy publication without an inventory pointer. The
	// available installation remains explicit; PATH cannot supply its pair.
	switch damaged {
	case "client":
		removeCompatibleControlExecutable(t, fixture.target.ClientExecutable)
	case "daemon":
		removeCompatibleControlExecutable(t, fixture.target.DaemonExecutable)
	case "both":
		removeCompatibleControlExecutable(t, fixture.target.ClientExecutable)
		removeCompatibleControlExecutable(t, fixture.target.DaemonExecutable)
	case "nonverifying":
		if err := os.WriteFile(fixture.target.ClientExecutable, []byte("changed owned fixture bytes"), 0o700); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown fixture damage %q", damaged)
	}
	return transport
}

func removeCompatibleControlExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func requireCompatibleControlClient(t *testing.T, client *pueue.Client, err error, saved, runtime task.SupervisorRef) {
	t.Helper()
	// The fixture's expected paths/config remain independent; runtime transport
	// always uses the installed-path representation, never historical byte SHAs.
	runtime.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	runtime.ClientSHA256, runtime.DaemonSHA256 = "", ""
	if client != nil {
		defer func() {
			if closeErr := client.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if err != nil || client == nil {
		t.Fatalf("passive compatible control required preparation: %v", err)
	}
	if client.Binding() != saved || client.RuntimeBinding() != runtime {
		t.Fatalf("control identity changed: saved=%+v runtime=%+v", client.Binding(), client.RuntimeBinding())
	}
	if _, err = client.Snapshot(t.Context()); err != nil {
		t.Fatal(fmt.Errorf("compatible client cannot observe status: %w", err))
	}
}
