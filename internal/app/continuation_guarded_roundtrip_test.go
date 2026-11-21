package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// This composes public guarded continuation and result reads with production
// stores. Both turns use the existing canonical sealing fixture; no provider
// process, authenticated guardian, or native execution closure is represented.
func TestGuardedContinuationPublicRoundTripAndStableIDReplay(t *testing.T) {
	testCase, found := recordedEnvironmentCaseNamed("codex/read-only")
	if !found {
		t.Fatal("Codex read-only fixture is unavailable")
	}
	fixture := newRecordedEnvironmentFixture(t, testCase)
	predecessor := newGuardedRoundTripPredecessor(t, fixture)
	fresh := predecessor.origin
	fresh.OperationID = "successor"
	fresh.GatewayRequestSHA256 = strings.Repeat("b", 64)
	arguments := guardedContinuationCLIArguments(t, predecessor, predecessor.origin, fresh)
	response := runGuardedRoundTripCLI(t, fixture.deps, arguments)
	const successorID = "ffffffffffffffffffffffffffffffff"
	receipt := requireGuardedRoundTripAdmission(t, response, successorID, fresh)
	assertGuardedRoundTripInheritance(t, fixture, predecessor.request, successorID, fresh)

	// The finite supervisor exposes the admitted row without running a worker.
	// Replay must reuse it before the sealing fixture supplies terminal evidence.
	writeRunnerStatus(t, fixture.statusPath, predecessor.request.RootID, successorID, 7)
	replay := runGuardedRoundTripCLI(t, fixture.deps, arguments)
	if replay.TaskID != successorID || replay.TaskRecord != "created" || replay.Connector == nil || !reflect.DeepEqual(replay.Connector.Receipt, receipt) {
		t.Fatalf("stable-ID replay changed the successor: %+v", replay)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "enqueue", 1)

	successor, err := predecessor.store.OpenTask(successorID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := successor.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	_, meta, err := successor.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	fixture.sealPredecessor(t, successor, meta.Predicate)
	assertGuardedRoundTripResult(t, fixture.deps, predecessor, successor, fresh)
	fixture.assertPredecessorUnchanged(t)
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
}

func requireGuardedRoundTripAdmission(t *testing.T, response Response, id string, origin task.GatewayOrigin) *taskdir.ConnectorReceipt {
	t.Helper()
	if response.TaskRecord != "created" || response.TaskID != id || response.Admission != task.AdmissionAdmitted.String() || response.Connector == nil || response.Connector.Receipt == nil {
		t.Fatalf("guarded continuation omitted the stable successor receipt: %+v", response)
	}
	receipt := response.Connector.Receipt
	if receipt.TaskID != id || receipt.Origin != origin {
		t.Fatalf("successor receipt retained predecessor correlation: %+v", receipt)
	}
	return receipt
}

// The store owns immutable task/context records. This setup is their only
// writer; public continuation and result commands read the same durable owner.
func newGuardedRoundTripPredecessor(t *testing.T, fixture *recordedEnvironmentFixture) *connectorFixture {
	t.Helper()
	store, err := openStore(fixture.root, fixture.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = store.EnableWorkspaceConnectorContext(t.Context(), true, true); err != nil {
		t.Fatal(err)
	}
	root, err := store.RootTokenContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	origin := task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "predecessor", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
	if _, err = store.OriginBarrierContext(t.Context(), origin.GatewayID, "initial", "", origin.AdmissionGeneration, root); err != nil {
		t.Fatal(err)
	}
	inventory, err := store.OriginInventoryContext(t.Context(), origin.GatewayID, origin.AdmissionGeneration, "", 10, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.OriginReopenContext(t.Context(), origin.GatewayID, "initial", origin.AdmissionGeneration, inventory.CompletenessReceipt, root); err != nil {
		t.Fatal(err)
	}
	project, err := taskdir.ObservePathProjectContext(t.Context(), fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	projectToken, err := taskdir.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("inherit this exact predecessor brief\n")
	request, err := admission.BuildRequest(store.RootID, strings.Repeat("b", 32), fixture.testCase.provider, fixture.workspace, task.TaskConfig{
		Permission: fixture.testCase.mode, Budget: "1m0s", Model: "fixture-model", Effort: "high", OutputSilenceTimeout: "30s",
	}, brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Origin = origin.Clone()
	request.ConnectorContext = &task.ConnectorContext{
		RootToken: root, Project: project,
		WorkspaceConstraint: &task.WorkspaceConstraint{Token: projectToken, RelativeProject: "."},
	}
	candidate, err := fixture.deps.Catalog.Candidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	profile, err := admission.ProfileForAdmission(candidate, request, true)
	if err != nil {
		t.Fatal(err)
	}
	meta := tasklifecycle.NewMeta(request, profile, fixture.supervisor.Binding(), fixture.runner, task.RunnerOwnershipManaged, "guarded-round-trip-test")
	td, err := store.CreateTaskContext(t.Context(), request.TaskID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fixture.sealPredecessor(t, td, meta.Predicate)
	fixture.predecessorID, fixture.predecessor = request.TaskID, readImmutableRecords(t, td.Dir)
	return &connectorFixture{store: store, task: td, request: &request, origin: origin, rootToken: root, projectToken: projectToken, deps: fixture.deps}
}

func assertGuardedRoundTripInheritance(t *testing.T, fixture *recordedEnvironmentFixture, predecessor *task.TaskRecord, id string, fresh task.GatewayOrigin) {
	t.Helper()
	request, _ := fixture.loadRecords(t, id)
	if request.ConnectorContext == nil || request.ConnectorContext.Project.Kind != task.PathDirectoryBinding {
		t.Fatalf("successor lost the path-bound execution context: %+v", request.ConnectorContext)
	}
	if request.Provider != predecessor.Provider || request.CanonicalCwd != predecessor.CanonicalCwd || request.Mode != predecessor.Mode || !reflect.DeepEqual(request.RequestedConfig, predecessor.RequestedConfig) {
		t.Fatalf("successor changed inherited execution configuration: %+v", request)
	}
	if !reflect.DeepEqual(request.ConnectorContext, predecessor.ConnectorContext) || !reflect.DeepEqual(request.Workspace, predecessor.Workspace) {
		t.Fatalf("successor changed inherited workspace/context: %+v", request)
	}
	if !reflect.DeepEqual(request.Origin, &fresh) || reflect.DeepEqual(request.Origin, predecessor.Origin) {
		t.Fatalf("successor did not use fresh correlation: %+v", request.Origin)
	}
	assertGuardedRoundTripInput(t, fixture, predecessor, request)
}

func assertGuardedRoundTripInput(t *testing.T, fixture *recordedEnvironmentFixture, predecessor, successor *task.TaskRecord) {
	t.Helper()
	if prior := successor.PriorSession; prior == nil || prior.Provider != predecessor.Provider || prior.ConversationID != fixture.session || prior.PredecessorTaskID != predecessor.TaskID {
		t.Fatalf("successor lost the exact predecessor session: %+v", prior)
	}
	records := readImmutableRecords(t, fixture.taskDir(successor.TaskID))
	if !bytes.Equal(records["brief.md"], fixture.predecessor["brief.md"]) || successor.BriefSHA256 != predecessor.BriefSHA256 || successor.BriefLength != predecessor.BriefLength {
		t.Fatal("omitted follow-up brief did not inherit the exact saved bytes")
	}
}

func assertGuardedRoundTripResult(t *testing.T, deps Dependencies, predecessor *connectorFixture, successor *taskdir.TaskDir, origin task.GatewayOrigin) {
	t.Helper()
	_, _, spec, _, err := successor.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	originPath := filepath.Join(t.TempDir(), "successor-origin.json")
	encoded, err := task.MarshalCanonical(origin)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, originPath, encoded)
	hash := sha256.Sum256([]byte(predecessor.projectToken))
	guards := []string{
		"--root", predecessor.store.Root, "--expected-root", predecessor.rootToken,
		"--expected-project-sha256", hex.EncodeToString(hash[:]), "--expected-spec-sha256", spec, "--expected-origin", originPath, "--json",
	}
	collected := runGuardedRoundTripCLI(t, deps, append([]string{"collect", successor.TaskID}, guards...))
	if collected.Publication != "committed" || collected.Outcome == nil || collected.Outcome.Verdict != task.VerdictCommitted || collected.Payload == nil {
		t.Fatalf("fixture-sealed successor was not accepted by public collect: %+v", collected)
	}
	descriptorPath := filepath.Join(t.TempDir(), "payload.json")
	encoded, err = task.MarshalCanonical(*collected.Payload)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, descriptorPath, encoded)
	arguments := append([]string{"result", "read", successor.TaskID, "--expected-descriptor", descriptorPath, "--limit", "64"}, guards...)
	result := runGuardedRoundTripCLI(t, deps, arguments)
	if result.Publication != "committed" || result.Connector == nil || result.Connector.ResultRange == nil || string(result.Connector.ResultRange.Data) != "answer" {
		t.Fatalf("guarded result did not return the fixture answer: %+v", result)
	}
}

func runGuardedRoundTripCLI(t *testing.T, deps Dependencies, arguments []string) Response {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(arguments, &stdout, &stderr, deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("public %s exit=%d stdout=%q stderr=%q: %v", arguments[0], code, &stdout, &stderr, err)
	}
	if code != 0 || response.Failure != nil {
		t.Fatalf("public %s exit=%d response=%s stderr=%q", arguments[0], code, &stdout, &stderr)
	}
	return response
}
