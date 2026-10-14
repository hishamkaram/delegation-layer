package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPrivateSubmissionGateRejectsCapturedGenerationAfterPublishedSuccessor(t *testing.T) {
	fixture := newHistoricalSupervisorFixture(t, false)
	generationA := fixture.predecessor
	generationB := fixture.successor
	assertHistoricalActiveSupervisor(t, fixture)
	assertHistoricalMigrationComplete(t, fixture)

	if err := publishPrivateOrdinaryScheduling(fixture.root, generationB, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
		t.Fatalf("publish generation B scheduling receipt: %v", err)
	}
	recorded, err := privateOrdinarySchedulingRecorded(fixture.root, generationB)
	if err != nil || !recorded {
		t.Fatalf("generation B scheduling receipt was not durable: recorded=%t err=%v", recorded, err)
	}

	gate := privateSubmissionGate(fixture.root)
	if _, gateErr := gate(t.Context(), generationA); !errors.Is(gateErr, pueue.ErrBinding) {
		t.Fatalf("captured generation A was admitted after generation B publication: %v", gateErr)
	}
	authorization, err := gate(t.Context(), generationB)
	if err != nil {
		t.Fatalf("published complete generation B was refused: %v", err)
	}
	if authorization.StagedLaunchCapability == nil || !authorization.StagedLaunchCapability.AddStashed || !authorization.StagedLaunchCapability.EnqueueByID {
		t.Fatalf("published generation B lost staged capability evidence: %+v", authorization)
	}
}

func TestPrivateSubmissionGateRejectsIncompleteStagedCapabilityEvidence(t *testing.T) {
	store, deps, _ := newSupervisorProbeTestFixture(t)
	supervisor, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, supervisor, err)
	if err != nil || supervisor == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", supervisor, err)
	}
	binding := supervisor.Binding()
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	path := privateOrdinarySchedulingTestPath(store.Root)
	data := readActiveGenerationFile(t, path)
	var record map[string]json.RawMessage
	if err = task.DecodeStrict(data, &record); err != nil {
		t.Fatal(err)
	}
	capability, err := task.MarshalCanonical(pueue.StagedLaunchCapability{AddStashed: true})
	if err != nil {
		t.Fatal(err)
	}
	record["staged_launch"] = capability
	data, err = task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	authorization, gateErr := privateSubmissionGate(store.Root)(t.Context(), binding)
	if !errors.Is(gateErr, task.ErrEvidenceFault) || authorization.StagedLaunchCapability != nil {
		t.Fatalf("incomplete staged capability was admitted: authorization=%+v err=%v", authorization, gateErr)
	}
}

func TestPrivateAdmissionUsesPublishedGenerationWithStagedSuccessor(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	candidate, pairFiles := stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	if candidate == active {
		t.Fatal("successor fixture did not change executable provenance")
	}
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root, pairFiles)
	clearActiveGenerationSupervisorCommands(t, filepath.Dir(active.ConfigPath))

	for range 4 {
		admitted, admissionErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
		settleSupervisorProbeTestClient(t, admitted, admissionErr)
		if admissionErr != nil || admitted == nil {
			t.Fatalf("warm admission with staged successor failed: client=%v err=%v", admitted, admissionErr)
		}
		if got := admitted.Binding(); got != active {
			t.Fatalf("warm admission selected staged successor: got=%+v want active=%+v", got, active)
		}
		if closeErr := admitted.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}

	assertActiveGenerationNoSupervisorCommands(t, filepath.Dir(active.ConfigPath))
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(active.ConfigPath), 1)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, activeReceipt) {
		t.Fatalf("warm admission changed R(A): got=%s want=%s", got, activeReceipt)
	}
}

func TestPrivateAdmissionRepairsStagedReceiptAgainstPublishedGeneration(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	candidate, pairFiles := stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	writeActiveGenerationSchedulingReceipt(t, store.Root, candidate)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root, pairFiles)
	clearActiveGenerationSupervisorCommands(t, filepath.Dir(active.ConfigPath))

	admitted, admissionErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, admitted, admissionErr)
	if admissionErr != nil || admitted == nil {
		t.Fatalf("stale-receipt repair failed: client=%v err=%v", admitted, admissionErr)
	}
	if got := admitted.Binding(); got != active {
		t.Fatalf("repair changed the active generation: got=%+v want=%+v", got, active)
	}
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, activeReceipt) {
		t.Fatalf("repair did not publish R(A): got=%s want=%s", got, activeReceipt)
	}
	assertActiveGenerationNoStatusOrDaemonRestart(t, filepath.Dir(active.ConfigPath), 1)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	if closeErr := admitted.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	clearActiveGenerationSupervisorCommands(t, filepath.Dir(active.ConfigPath))
	warm, warmErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, warm, warmErr)
	if warmErr != nil || warm == nil || warm.Binding() != active {
		t.Fatalf("post-repair warm admission = client=%v binding=%+v err=%v", warm, bindingOfActiveGenerationClient(warm), warmErr)
	}
	assertActiveGenerationNoSupervisorCommands(t, filepath.Dir(active.ConfigPath))
	if closeErr := warm.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestPrivateAdmissionConcurrentStaleReceiptElectsOneRepairOwner(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	candidate, pairFiles := stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	writeActiveGenerationSchedulingReceipt(t, store.Root, candidate)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root, pairFiles)
	base := filepath.Dir(active.ConfigPath)
	clearActiveGenerationSupervisorCommands(t, base)

	for _, admitted := range prepareSupervisorProbeAdmissions(t, store, deps, 16) {
		if got := admitted.Binding(); got != active {
			t.Fatalf("concurrent admission selected non-active generation: got=%+v want=%+v", got, active)
		}
		if closeErr := admitted.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	parallel := readActiveGenerationFile(t, filepath.Join(base, "parallel"))
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 1 {
		t.Fatalf("stale receipt repair count=%d, want one owner; log=%q", count, parallel)
	}
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestGroupCount(t, base, 3)
	assertSupervisorProbeTestStagedCapabilityCount(t, base, 1)
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, activeReceipt) {
		t.Fatalf("concurrent repair did not leave R(A): got=%s", got)
	}
}

func TestPrivateAdmissionMalformedSchedulingReceiptIsNotOverwritten(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	_, pairFiles := stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	malformed := []byte(`{"schema_version":`)
	if err = os.WriteFile(privateOrdinarySchedulingTestPath(store.Root), malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root, pairFiles)
	clearActiveGenerationSupervisorCommands(t, filepath.Dir(active.ConfigPath))

	admitted, admissionErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, admitted, admissionErr)
	if admitted != nil {
		if closeErr := admitted.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if admissionErr == nil || !errors.Is(admissionErr, pueue.ErrBinding) {
		t.Fatalf("malformed scheduling receipt error=%v, want binding error", admissionErr)
	}
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, malformed) {
		t.Fatalf("malformed receipt was overwritten: got=%s want=%s", got, malformed)
	}
	assertActiveGenerationNoSupervisorCommands(t, filepath.Dir(active.ConfigPath))
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
}

func TestPrivateAdmissionRepairsValidIncompleteSchedulingReceipt(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	_, pairFiles := stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	writeIncompleteActiveGenerationSchedulingReceipt(t, store.Root, active)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root, pairFiles)
	base := filepath.Dir(active.ConfigPath)
	clearActiveGenerationSupervisorCommands(t, base)

	admitted, admissionErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, admitted, admissionErr)
	if admissionErr != nil || admitted == nil || admitted.Binding() != active {
		t.Fatalf("incomplete-receipt repair returned client=%v binding=%+v err=%v", admitted, bindingOfActiveGenerationClient(admitted), admissionErr)
	}
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, activeReceipt) {
		t.Fatalf("incomplete receipt was not upgraded to full R(A): got=%s want=%s", got, activeReceipt)
	}
	assertActiveGenerationNoStatusOrDaemonRestart(t, base, 1)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	if closeErr := admitted.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestPrivateAdmissionUpgradesCapabilitylessGenerationReceiptOnce(t *testing.T) {
	store, deps, _ := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	writeCapabilitylessActiveGenerationSchedulingReceipt(t, store.Root)
	base := filepath.Dir(active.ConfigPath)
	clearActiveGenerationSupervisorCommands(t, base)

	admitted, admissionErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, admitted, admissionErr)
	if admissionErr != nil || admitted == nil || admitted.Binding() != active {
		t.Fatalf("capabilityless receipt upgrade returned client=%v binding=%+v err=%v", admitted, bindingOfActiveGenerationClient(admitted), admissionErr)
	}
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, activeReceipt) {
		t.Fatalf("capabilityless receipt was not upgraded to current evidence: got=%s want=%s", got, activeReceipt)
	}
	assertActiveGenerationNoStatusOrDaemonRestart(t, base, 1)
	if closeErr := admitted.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	clearActiveGenerationSupervisorCommands(t, base)
	warm, warmErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, warm, warmErr)
	if warmErr != nil || warm == nil || warm.Binding() != active {
		t.Fatalf("warm admission after capability upgrade = client=%v binding=%+v err=%v", warm, bindingOfActiveGenerationClient(warm), warmErr)
	}
	assertActiveGenerationNoSupervisorCommands(t, base)
	if closeErr := warm.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestPrivateStatusRepairUpgradesCapabilitylessGenerationReceipt(t *testing.T) {
	store, deps, _ := newSupervisorProbeTestFixture(t)
	supervisor, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, supervisor, err)
	if err != nil || supervisor == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", supervisor, err)
	}
	want := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	writeCapabilitylessActiveGenerationSchedulingReceipt(t, store.Root)
	base := filepath.Dir(supervisor.Binding().ConfigPath)
	clearActiveGenerationSupervisorCommands(t, base)

	controlContext, cancel := context.WithTimeout(t.Context(), pueue.PrivateStatusObservationTimeout)
	err = ensurePrivateAdmissionSchedulingContext(controlContext, store.Root, supervisor)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, want) {
		t.Fatalf("status repair did not upgrade scheduling receipt: got=%s want=%s", got, want)
	}
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestGroupCount(t, base, 2)
	assertSupervisorProbeTestStagedCapabilityCount(t, base, 1)
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
	parallel, readErr := os.ReadFile(filepath.Join(base, "parallel"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 1 {
		t.Fatalf("status scheduling repair count=%d, want one; log=%q", count, parallel)
	}
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestPrivateAdmissionRestartsExactStoppedPublishedGeneration(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	_, pairFiles := stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root, pairFiles)
	stopActiveGenerationEndpoint(t, active)

	admitted, admissionErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, admitted, admissionErr)
	if admissionErr != nil || admitted == nil || admitted.Binding() != active {
		t.Fatalf("stopped published generation restart returned client=%v binding=%+v err=%v", admitted, bindingOfActiveGenerationClient(admitted), admissionErr)
	}
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(active.ConfigPath), 2)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, activeReceipt) {
		t.Fatalf("exact-generation restart changed R(A): got=%s want=%s", got, activeReceipt)
	}
	if closeErr := admitted.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestPrivateAdmissionRefusesUnavailablePublishedPairWithStagedSuccessor(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	_, pairFiles := stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root, pairFiles)
	stopActiveGenerationEndpoint(t, active)
	if err = os.Remove(active.ClientExecutable); err != nil {
		t.Fatal(err)
	}

	admitted, admissionErr := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, admitted, admissionErr)
	if admitted != nil {
		t.Fatalf("unavailable published pair returned a supervisor: %+v", admitted.Binding())
	}
	if !errors.Is(admissionErr, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unavailable published pair error=%v, want migration required", admissionErr)
	}
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(active.ConfigPath), 1)
	assertPublishedGenerationFilesUnchanged(t, activeFiles)
	if got := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root)); !bytes.Equal(got, activeReceipt) {
		t.Fatalf("refused recovery changed R(A): got=%s want=%s", got, activeReceipt)
	}
}

func TestPrivateAdmissionWithoutPublishedGenerationBootstrapsStagedPair(t *testing.T) {
	store, deps, clientPath, daemonPath := newFreshStagedGenerationFixture(t)
	base := filepath.Join(store.Root, ".supervisor")
	if _, err := os.Stat(filepath.Join(base, "supervisor.identity.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh fixture unexpectedly has a published generation: %v", err)
	}

	admitted, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, admitted, err)
	if err != nil || admitted == nil {
		t.Fatalf("fresh staged-pair admission failed: client=%v err=%v", admitted, err)
	}
	if !strings.Contains(admitted.Binding().ClientExecutable, stateSupervisorPairDirectoryPrefix) {
		t.Fatalf("fresh admission did not bootstrap the staged state-root pair: %s", admitted.Binding().ClientExecutable)
	}
	wantClientSHA, err := commonprovider.FingerprintExecutable(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	wantDaemonSHA, err := commonprovider.FingerprintExecutable(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Binding().ClientSHA256 != wantClientSHA || admitted.Binding().DaemonSHA256 != wantDaemonSHA {
		t.Fatalf("fresh admission used the wrong staged pair: binding=%+v", admitted.Binding())
	}
	complete, err := pueue.PrivateSupervisorMigrationComplete(store.Root, admitted.Binding())
	if err != nil || !complete {
		t.Fatalf("fresh staged generation is incomplete: complete=%t err=%v", complete, err)
	}
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
	if closeErr := admitted.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func stagePublishedGenerationSuccessor(t *testing.T, root string, active task.SupervisorRef, templateClient, templateDaemon string) (task.SupervisorRef, []string) {
	t.Helper()
	sourceDirectory := canonicalAppTestTempDir(t)
	clientSource := filepath.Join(sourceDirectory, "pueue-b")
	daemonSource := filepath.Join(sourceDirectory, "pueued-b")
	copyActiveGenerationExecutable(t, templateClient, clientSource, "# staged successor client")
	copyActiveGenerationExecutable(t, templateDaemon, daemonSource, "# staged successor daemon")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := installStateSupervisorPairForAdmission(ctx, root, clientSource, daemonSource); err != nil {
		t.Fatalf("stage successor supervisor pair: %v", err)
	}
	base := filepath.Join(root, ".supervisor")
	client, daemon, found, err := resolveStateSupervisorPair(base)
	if err != nil || !found {
		t.Fatalf("resolve staged successor pair: client=%s daemon=%s found=%t err=%v", client, daemon, found, err)
	}
	clientSHA, err := commonprovider.FingerprintExecutable(client)
	if err != nil {
		t.Fatal(err)
	}
	daemonSHA, err := commonprovider.FingerprintExecutable(daemon)
	if err != nil {
		t.Fatal(err)
	}
	candidate := active
	candidate.ClientExecutable = client
	candidate.ClientSHA256 = clientSHA
	candidate.DaemonExecutable = daemon
	candidate.DaemonSHA256 = daemonSHA
	pairDirectory := filepath.Dir(client)
	return candidate, []string{
		filepath.Join(base, stateSupervisorPairRecordName),
		filepath.Join(pairDirectory, "pueue"),
		filepath.Join(pairDirectory, "pueued"),
	}
}

func newFreshStagedGenerationFixture(t *testing.T) (*taskdir.Store, Dependencies, string, string) {
	t.Helper()
	templateStore, templateDeps, templateClient := newSupervisorProbeTestFixture(t)
	root := filepath.Join(canonicalAppTestTempDir(t), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	sourceDirectory := canonicalAppTestTempDir(t)
	clientPath := filepath.Join(sourceDirectory, "pueue-b")
	daemonPath := filepath.Join(sourceDirectory, "pueued-b")
	copyActiveGenerationExecutable(t, templateClient.Binding().ClientExecutable, clientPath, templateStore.RootID+" -> "+store.RootID)
	copyActiveGenerationExecutable(t, templateClient.Binding().DaemonExecutable, daemonPath, templateStore.RootID+" -> "+store.RootID)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err = installStateSupervisorPairForAdmission(ctx, root, clientPath, daemonPath); err != nil {
		t.Fatalf("stage fresh successor pair: %v", err)
	}
	return store, Dependencies{InitialSupervisorExecutable: clientPath, SupervisorOptions: templateDeps.SupervisorOptions}, clientPath, daemonPath
}

func copyActiveGenerationExecutable(t *testing.T, source, destination, marker string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n# "+marker+"\n")...)
	if err = os.WriteFile(destination, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeActiveGenerationSchedulingReceipt(t *testing.T, root string, binding task.SupervisorRef) {
	t.Helper()
	path := privateOrdinarySchedulingTestPath(root)
	data := readActiveGenerationFile(t, path)
	var record map[string]json.RawMessage
	if err := task.DecodeStrict(data, &record); err != nil {
		t.Fatal(err)
	}
	encoded, err := task.MarshalCanonical(binding)
	if err != nil {
		t.Fatal(err)
	}
	record["supervisor"] = encoded
	data, err = task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeIncompleteActiveGenerationSchedulingReceipt(t *testing.T, root string, binding task.SupervisorRef) {
	t.Helper()
	data, err := task.MarshalCanonical(map[string]any{
		"schema_version": task.SchemaVersion,
		"revision":       privateOrdinarySchedulingDaemonRevision,
		"supervisor": map[string]string{
			"executable": binding.DaemonExecutable,
			"sha256":     binding.DaemonSHA256,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(privateOrdinarySchedulingTestPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCapabilitylessActiveGenerationSchedulingReceipt(t *testing.T, root string) {
	t.Helper()
	path := privateOrdinarySchedulingTestPath(root)
	data := readActiveGenerationFile(t, path)
	var record map[string]json.RawMessage
	if err := task.DecodeStrict(data, &record); err != nil {
		t.Fatal(err)
	}
	revision, err := json.Marshal(privateOrdinarySchedulingCapabilitylessRevision)
	if err != nil {
		t.Fatal(err)
	}
	record["revision"] = revision
	delete(record, "staged_launch")
	data, err = task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func stopActiveGenerationEndpoint(t *testing.T, binding task.SupervisorRef) {
	t.Helper()
	base := filepath.Dir(binding.ConfigPath)
	for _, path := range []string{
		filepath.Join(base, "ready"),
		filepath.Join(base, "run", "pueue.pid"),
		strings.TrimPrefix(binding.Endpoint, "unix:"),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func privateOrdinarySchedulingTestPath(root string) string {
	return filepath.Join(root, ".supervisor", "ordinary-scheduling.json")
}

func snapshotPublishedGenerationFiles(t *testing.T, root string, pairFiles []string) map[string][]byte {
	t.Helper()
	base := filepath.Join(root, ".supervisor")
	paths := append([]string{
		filepath.Join(base, "supervisor.identity.json"),
		filepath.Join(base, "daemon.identity.json"),
	}, pairFiles...)
	snapshot := make(map[string][]byte, len(paths))
	for _, path := range paths {
		snapshot[path] = readActiveGenerationFile(t, path)
	}
	return snapshot
}

func assertPublishedGenerationFilesUnchanged(t *testing.T, snapshot map[string][]byte) {
	t.Helper()
	for path, want := range snapshot {
		if got := readActiveGenerationFile(t, path); !bytes.Equal(got, want) {
			t.Fatalf("published generation file changed: path=%s got=%q want=%q", path, got, want)
		}
	}
}

func clearActiveGenerationSupervisorCommands(t *testing.T, base string) {
	t.Helper()
	clearSupervisorProbeTestStatus(t, base)
	clearSupervisorProbeTestGroup(t, base)
	if err := os.WriteFile(filepath.Join(base, "parallel"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "staged-capability"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertActiveGenerationNoSupervisorCommands(t *testing.T, base string) {
	t.Helper()
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestGroupCount(t, base, 0)
	assertSupervisorProbeTestStagedCapabilityCount(t, base, 0)
	parallel, err := os.ReadFile(filepath.Join(base, "parallel"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(parallel) != 0 {
		t.Fatalf("private admission ran a scheduling command: %q", parallel)
	}
}

func assertActiveGenerationNoStatusOrDaemonRestart(t *testing.T, base string, daemonStarts int) {
	t.Helper()
	assertSupervisorProbeTestNoStatus(t, base)
	assertSupervisorProbeTestGroupCount(t, base, 3)
	assertSupervisorProbeTestStagedCapabilityCount(t, base, 1)
	assertSupervisorProbeTestDaemonStartCount(t, base, daemonStarts)
	parallel, err := os.ReadFile(filepath.Join(base, "parallel"))
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(parallel, []byte("parallel\n")); count != 1 {
		t.Fatalf("ordinary scheduling repair count=%d, want one; log=%q", count, parallel)
	}
}

func assertSupervisorProbeTestStagedCapabilityCount(t *testing.T, base string, want int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, "staged-capability"))
	if errors.Is(err, os.ErrNotExist) && want == 0 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if add, enqueue := bytes.Count(data, []byte("add-help\n")), bytes.Count(data, []byte("enqueue-help\n")); add != want || enqueue != want {
		t.Fatalf("staged capability probes add=%d enqueue=%d, want %d each; log=%q", add, enqueue, want, data)
	}
}

func readActiveGenerationFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func bindingOfActiveGenerationClient(client *pueue.Client) task.SupervisorRef {
	if client == nil {
		return task.SupervisorRef{}
	}
	return client.Binding()
}
