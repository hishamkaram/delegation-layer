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

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPrivateSubmissionGateRejectsCapturedGenerationAfterPublishedSuccessor(t *testing.T) {
	// These gate checks consume completed canonical record DATA. They do not
	// require a persistent daemon or a newly executed migration.
	store := newBareInspectionStore(t)
	generationA := supervisorGenerationTestBinding(store.Root, "A", true)
	generationB := supervisorGenerationTestBinding(store.Root, "B", true)
	writeSupervisorGenerationTestPrivateGeneration(t, store, generationB)
	writeSupervisorGenerationTestHistory(t, store, generationA, generationB)
	fixture := &historicalSupervisorFixture{root: store.Root, predecessor: generationA, successor: generationB}
	assertHistoricalActiveSupervisor(t, fixture)
	assertHistoricalMigrationComplete(t, fixture)

	if err := writePrivateSchedulingFixture(fixture.root, generationB, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
		t.Fatalf("publish generation B scheduling receipt: %v", err)
	}
	recorded, err := supervisorcore.OrdinarySchedulingRecorded(fixture.root, generationB)
	if err != nil || !recorded {
		t.Fatalf("generation B scheduling receipt was not durable: recorded=%t err=%v", recorded, err)
	}

	gate := supervisorcore.SubmissionGate(fixture.root)
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
	f := newPublicContinuationFixture(t)
	store, binding := f.owner.store, f.client.Binding()
	if closeErr := f.client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	path := privateOrdinarySchedulingTestPath(store.Root)
	data := readActiveGenerationFile(t, path)
	var record map[string]json.RawMessage
	if err := task.DecodeStrict(data, &record); err != nil {
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

	authorization, gateErr := supervisorcore.SubmissionGate(store.Root)(t.Context(), binding)
	if !errors.Is(gateErr, task.ErrEvidenceFault) || authorization.StagedLaunchCapability != nil {
		t.Fatalf("incomplete staged capability was admitted: authorization=%+v err=%v", authorization, gateErr)
	}
}

func TestPrivateAdmissionUsesPublishedGenerationWithStagedSuccessor(t *testing.T) {
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
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

	candidate := installedGenerationCandidate(t, store, active)
	if candidate == active {
		t.Fatal("successor fixture did not change executable provenance")
	}
	deps.InitialSupervisorExecutable = candidate.ClientExecutable
	deps = withSupervisorProbeRouting(t, deps, owner)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
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
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
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
	candidate := installedGenerationCandidate(t, store, active)
	writeActiveGenerationSchedulingReceipt(t, store.Root, candidate)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
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
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	candidate := installedGenerationCandidate(t, store, active)
	activeReceipt := readActiveGenerationFile(t, privateOrdinarySchedulingTestPath(store.Root))
	writeActiveGenerationSchedulingReceipt(t, store.Root, candidate)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
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
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	malformed := []byte(`{"schema_version":`)
	if err = os.WriteFile(privateOrdinarySchedulingTestPath(store.Root), malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
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
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
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
	writeIncompleteActiveGenerationSchedulingReceipt(t, store.Root, active)
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
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
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
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
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
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
	err = supervisorcore.EnsureAdmissionSchedulingContext(controlContext, store.Root, supervisor)
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
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
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
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
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

func TestPrivateAdmissionRefusesUnavailablePublishedPair(t *testing.T) {
	for _, missingRole := range []string{"client", "daemon"} {
		t.Run(missingRole+" missing", func(t *testing.T) {
			testPrivateAdmissionUnavailablePublishedRole(t, missingRole)
		})
	}
}

func testPrivateAdmissionUnavailablePublishedRole(t *testing.T, missingRole string) {
	t.Helper()
	store, deps, _, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
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
	activeFiles := snapshotPublishedGenerationFiles(t, store.Root)
	stopOwnedActiveGenerationEndpoint(t, owner, active, deps.SupervisorOptions)
	missingPath := active.ClientExecutable
	if missingRole == "daemon" {
		missingPath = active.DaemonExecutable
	}
	if err = os.Remove(missingPath); err != nil {
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

// The component daemon is finite. Join its actual Wait before closing its
// independently owned IPC listener and clearing stopped-fixture DATA markers.
// This models a stopped component; it is not a native Pueue shutdown proof.
func stopOwnedActiveGenerationEndpoint(t *testing.T, owner *componentSupervisorFixture, binding task.SupervisorRef, options pueue.Options) {
	t.Helper()
	ctx, cancel := supervisorcore.ControlContextWithParent(t.Context(), options)
	defer cancel()
	waits := owner.daemonWaits()
	if len(waits) != 1 {
		t.Fatalf("fixture daemon Wait count = %d, want one actual Start", len(waits))
	}
	if err := awaitComponentDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(binding.Endpoint, "unix:")
	for _, endpoint := range owner.endpoints {
		if endpoint.listener == nil || endpoint.listener.Addr().Network() != "unix" || endpoint.listener.Addr().String() != path {
			continue
		}
		closeErr := endpoint.listener.Close()
		var waitErr error
		select {
		case <-endpoint.done:
		case <-ctx.Done():
			waitErr = ctx.Err()
		}
		if err := errors.Join(closeErr, waitErr); err != nil {
			t.Fatal(err)
		}
		// The listener has retired its owned socket. Only readiness and PID
		// DATA can remain for the existing stopped-component state helper.
		stopActiveGenerationEndpoint(t, binding)
		return
	}
	t.Fatal("active generation has no matching owned Unix listener")
}

func TestPrivateAdmissionWithoutPublishedGenerationBootstrapsStagedPair(t *testing.T) {
	store, deps, clientPath, daemonPath, owner := newFreshStagedGenerationFixture(t)
	base := filepath.Join(store.Root, ".supervisor")
	if _, err := os.Stat(filepath.Join(base, "supervisor.identity.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh fixture unexpectedly has a published generation: %v", err)
	}

	// Explicit canonical component setup, not public routing or native placement.
	before := len(owner.daemonWaits())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	admitted, err := supervisorcore.PreparePrivateAdmissionSupervisorContext(ctx, deps.supervisorLookup(), store, owner.setupOptions(deps.SupervisorOptions))
	owner.ownClient(admitted, err)
	owner.ownDaemonClients(admitted, before)
	if err != nil || admitted == nil {
		t.Fatalf("fresh staged-pair admission failed: client=%v err=%v", admitted, err)
	}
	if admitted.Binding().ClientExecutable != clientPath || admitted.Binding().DaemonExecutable != daemonPath || admitted.Binding().ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || admitted.Binding().ClientSHA256 != "" || admitted.Binding().DaemonSHA256 != "" {
		t.Fatalf("fresh generation did not use selected installed roles: %+v", admitted.Binding())
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

func installedGenerationCandidate(t *testing.T, store *taskdir.Store, active task.SupervisorRef) task.SupervisorRef {
	t.Helper()
	directory := testsupport.CanonicalTempDir(t)
	client, daemon := filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	writeSupervisorProbeExecutables(t, store.RootID, client, daemon)
	candidate := active
	candidate.ClientExecutable, candidate.DaemonExecutable = client, daemon
	candidate.ClientSHA256, candidate.DaemonSHA256 = "", ""
	candidate.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	return candidate
}

func newFreshStagedGenerationFixture(t *testing.T) (*taskdir.Store, Dependencies, string, string, *componentSupervisorFixture) {
	t.Helper()
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	store, err := taskdir.InitStore(owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(owner.base, "installed")
	client, daemon := filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	writeSupervisorProbeExecutables(t, store.RootID, client, daemon)
	return store, Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: client}, client, daemon, owner
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
	// Frozen historical receipt DATA, never derived from a program.
	data, err := task.MarshalCanonical(map[string]any{
		"schema_version": task.SchemaVersion,
		"revision":       "unlimited-v2",
		"supervisor": map[string]string{
			"executable": binding.DaemonExecutable,
			"sha256":     strings.Repeat("a", 64),
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
	revision, err := json.Marshal("unlimited-v3")
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

func snapshotPublishedGenerationFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	base := filepath.Join(root, ".supervisor")
	paths := []string{
		filepath.Join(base, "supervisor.identity.json"),
		filepath.Join(base, "daemon.identity.json"),
	}
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

func TestPrivateSubmissionGateAcceptsCompatibleTransportForPublishedGeneration(t *testing.T) {
	f := newPublicContinuationFixture(t)
	published := f.client.Binding()
	runtime := f.expectedRuntime(f.clientD, f.daemonD)
	before := snapshotPublishedGenerationFiles(t, f.owner.root)
	if err := writePrivateSchedulingFixture(f.owner.root, runtime, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
		t.Fatal(err)
	}
	authorization, err := checkPrivateSubmissionGate(t, f.owner.root, runtime)
	if err != nil || authorization.StagedLaunchCapability == nil || !authorization.StagedLaunchCapability.AddStashed || !authorization.StagedLaunchCapability.EnqueueByID {
		t.Fatalf("compatible transport was treated as the published daemon generation: %+v %v", authorization, err)
	}
	assertPublishedGenerationFilesUnchanged(t, before)
	if got, found, readErr := pueue.ReadPrivateSupervisorBinding(f.owner.root); readErr != nil || !found || got != published {
		t.Fatalf("gate changed canonical generation: found=%t binding=%+v error=%v", found, got, readErr)
	}
}

func checkPrivateSubmissionGate(t *testing.T, root string, runtime task.SupervisorRef) (pueue.SubmissionAuthorization, error) {
	t.Helper()
	var authorization pueue.SubmissionAuthorization
	err := pueue.WithSupervisorLeaseContext(t.Context(), root, func(ctx context.Context) error {
		var gateErr error
		authorization, gateErr = supervisorcore.SubmissionGate(root)(ctx, runtime)
		return gateErr
	})
	return authorization, err
}

func TestPrivateSubmissionGateCompatibleTransportPreservesIdentityGuards(t *testing.T) {
	for _, changed := range []string{"config", "resolved config", "endpoint", "canonical daemon", "mixed identity", "runtime receipt"} {
		t.Run(changed, func(t *testing.T) {
			f := newPublicContinuationFixture(t)
			runtime := f.expectedRuntime(f.clientD, f.daemonD)
			mutateCompatibleSubmissionEvidence(t, f, &runtime, changed)
			receipt := runtime
			if changed == "runtime receipt" {
				receipt = f.expectedRuntime(f.clientC, f.daemonC)
			}
			if err := writePrivateSchedulingFixture(f.owner.root, receipt, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
				t.Fatal(err)
			}
			before := snapshotPublishedGenerationFiles(t, f.owner.root)
			authorization, err := checkPrivateSubmissionGate(t, f.owner.root, runtime)
			if !errors.Is(err, pueue.ErrBinding) || authorization.StagedLaunchCapability != nil {
				t.Fatalf("compatible transport bypassed %s identity evidence: %+v %v", changed, authorization, err)
			}
			assertPublishedGenerationFilesUnchanged(t, before)
			f.assertNoContinuationWork(t, "")
		})
	}
}

func mutateCompatibleSubmissionEvidence(t *testing.T, f *publicContinuationFixture, runtime *task.SupervisorRef, changed string) {
	t.Helper()
	switch changed {
	case "config":
		runtime.ConfigDigest = task.ComputeSHA256([]byte("foreign config data"))
	case "resolved config":
		runtime.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("foreign resolved config data"))
	case "endpoint":
		runtime.Endpoint = "unix:" + filepath.Join(f.owner.root, ".supervisor", "foreign.socket")
	case "canonical daemon":
		writeConflictingLaunchWitness(t, f.owner.root, f.client.Binding())
	case "mixed identity":
		// Literal historical DATA combined with an installed-path marker is
		// invalid; it is never derived from installed executable contents.
		runtime.DaemonSHA256 = strings.Repeat("a", 64)
	case "runtime receipt":
		// The caller records C's capability while presenting runtime D.
	}
}
