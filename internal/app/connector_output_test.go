package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/resultread"
	schedulercontract "github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type connectorOutputUsageInterpreter struct{ model string }

func (connectorOutputUsageInterpreter) Reference() task.PredicateRef {
	return task.FixturePredicateRef()
}

func (p connectorOutputUsageInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, output io.Writer) (task.Interpretation, error) {
	result, err := predicate.FixtureV1().Evaluate(input, raw, output)
	result.Usage = []task.UsageMetadata{{Scope: task.UsageScopeTask, Source: task.UsageSourceProviderEnvelope, Reliability: task.UsageReliabilityReported, Model: &p.model}}
	return result, err
}

func TestConnectorOutputNearRecordLimitGuardedCollect(t *testing.T) {
	for _, binding := range []string{"", "opaque-bound-origin"} {
		t.Run(binding, func(t *testing.T) { requireConnectorOutputNearLimit(t, binding) })
	}
}

func requireConnectorOutputNearLimit(t *testing.T, binding string) {
	t.Helper()
	f := connectorOutputFixture(t, binding)
	model := sealConnectorOutputNearLimit(t, f)
	encoded, err := task.MarshalCanonical(f.origin)
	if err != nil {
		t.Fatal(err)
	}
	deps := f.deps
	deps.Input = bytes.NewReader(encoded)
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"collect", f.request.TaskID, "--root", f.store.Root, "--expected-root", f.rootToken,
		"--expected-project", f.projectToken, "--expected-origin", "-", "--json",
	}, &stdout, &stderr, deps)
	stored, err := os.ReadFile(filepath.Join(f.task.Dir, "outcome.json"))
	if err != nil || len(stored) != task.MaxControlRecordSize-1 {
		t.Fatalf("fixture did not create the legal near-limit outcome: bytes=%d err=%v", len(stored), err)
	}
	var outcome task.OutcomeRecord
	if err = task.DecodeStrict(stored, &outcome); err != nil {
		t.Fatal(err)
	}
	if err = task.ValidateOutcomeRecord(&outcome); err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("guarded collect could not present a valid near-limit outcome: exit=%d bytes=%d stderr=%q", code, stdout.Len(), &stderr)
	}
	var response Response
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Publication != "committed" || !reflect.DeepEqual(response.Outcome, &outcome) || *response.Outcome.Usage[0].Model != model {
		t.Fatal("guarded collect changed the canonical outcome or usage model")
	}
	// Collection naturally sets this duplicate auxiliary digest. Only binding
	// presentation removes it; the complete canonical Outcome above is retained.
	requireConnectorAuxiliaryDigest(t, binding != "", response.EvidenceSHA256, outcome.EvidenceSHA256)
	control, code := f.public(t, "resolve", f.request.TaskID)
	if code != 0 || !reflect.DeepEqual(control.Outcome, &outcome) || control.Connector == nil || control.Connector.Receipt == nil {
		t.Fatal("guarded control lost the near-limit outcome or Native receipt")
	}
}

func requireConnectorAuxiliaryDigest(t *testing.T, bound bool, actual, canonical string) {
	t.Helper()
	if bound {
		if actual != "" {
			t.Fatal("bound public collect did not apply the validated-Origin projection")
		}
		return
	}
	if actual != canonical {
		t.Fatal("legacy public collect lost its auxiliary evidence digest")
	}
}

func connectorOutputFixture(t *testing.T, binding string) *connectorFixture {
	t.Helper()
	f := newConnectorFixture(t)
	if binding == "" {
		return f
	}
	request, meta, err := f.task.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request.TaskID, meta.TaskID = strings.Repeat("e", 32), strings.Repeat("e", 32)
	f.origin.Binding = binding
	request.Origin = f.origin.Clone()
	td, err := f.store.CreateTask(request.TaskID, request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	f.task, f.request = td, request
	return f
}

func sealConnectorOutputNearLimit(t *testing.T, f *connectorFixture) string {
	t.Helper()
	_, _, spec, meta, err := f.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	model := ""
	shape := task.OutcomeRecord{
		SchemaVersion: task.SchemaVersion, RootID: f.request.RootID, TaskID: f.request.TaskID,
		SpecSHA256: spec, MetaSHA256: meta, Verdict: task.VerdictCommitted, EvidenceSHA256: strings.Repeat("a", 64),
		Predicate: task.FixturePredicateRef(), Payload: task.PayloadDescriptor{Basename: "result.txt", Length: 6, SHA256: task.ComputeSHA256([]byte("answer"))},
		Usage: []task.UsageMetadata{{Scope: task.UsageScopeTask, Source: task.UsageSourceProviderEnvelope, Reliability: task.UsageReliabilityReported, Model: &model}},
	}
	encoded, err := task.MarshalCanonical(shape)
	if err != nil {
		t.Fatal(err)
	}
	model = strings.Repeat("m", task.MaxControlRecordSize-1-len(encoded))
	registry, err := predicate.New(connectorOutputUsageInterpreter{model: model})
	if err != nil {
		t.Fatal(err)
	}
	f.deps.PredicateRegistry = func() predicate.Registry { return registry }
	permit, err := f.task.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	for _, operation := range []func() error{permit.Consume, func() error { return f.task.RecordStarted(0) }, func() error { return f.task.WriteRawFiles("answer", "") }} {
		if err = operation(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.task.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	return model
}

func TestConnectorOutputBindingProjectionKeepsEvidenceAndCopiesChangedValues(t *testing.T) {
	response := connectorOutputFixedShape()
	response.Outcome = &task.OutcomeRecord{Usage: []task.UsageMetadata{{Model: new("complete model")}}, Diagnostics: []task.OutputDiagnostic{{Message: "sealed diagnostic"}}}
	response.Raw = []taskdir.LogDescriptor{{Path: strings.Repeat("auxiliary", task.MaxControlRecordSize)}}
	response.Continuation = &ContinuationResponse{ContinueCommand: "unused command"}
	response.Capability = &CapabilityReport{Provider: "unused"}
	response.Team = &task.TeamContext{TeamName: "unused"}
	before := connectorOutputEncode(t, response)
	projected := projectConnectorOutput(response, connectorOutputBinding)
	if !reflectConnectorOutputEvidence(projected, response) {
		t.Fatal("binding projection changed retained evidence")
	}
	if projected.Raw != nil || projected.Continuation != nil || projected.Capability != nil || projected.Team != nil || projected.Stops != nil {
		t.Fatal("binding projection retained auxiliary observations")
	}
	if projected.Failure.RecoveryCommand != "" || response.Failure.RecoveryCommand == "" || len(projected.Connector.Features) != 5 || len(response.Connector.Features) != 10 {
		t.Fatal("projection did not isolate recovery/feature changes")
	}
	if !bytes.Equal(before, connectorOutputEncode(t, response)) {
		t.Fatal("projection changed the original response")
	}
	var stdout, stderr bytes.Buffer
	result := commandResult{response: response, code: 4, connectorOutput: connectorOutputBinding}
	if code := presentCommandResult(Arguments{Command: "collect", JSON: true, Connector: ConnectorArguments{ExpectedRoot: response.RootID}}, result, &stdout, &stderr); code != 4 {
		t.Fatalf("projection changed rejection exit code: %d %s", code, &stderr)
	}
	if !bytes.Equal(before, connectorOutputEncode(t, response)) {
		t.Fatal("JSON presentation mutated the original response")
	}
}

func reflectConnectorOutputEvidence(projected, original Response) bool {
	failure := *original.Failure
	failure.RecoveryCommand = ""
	return projected.Outcome == original.Outcome && projected.Payload == original.Payload && projected.ResultRange == original.ResultRange &&
		projected.Stop == original.Stop && projected.ExecutionAssurance == original.ExecutionAssurance &&
		projected.Connector.Receipt == original.Connector.Receipt && projected.Connector.ExecutionClosure == original.Connector.ExecutionClosure &&
		projected.Connector.OriginBarrier == original.Connector.OriginBarrier && projected.Connector.ResultRange == original.Connector.ResultRange &&
		reflect.DeepEqual(projected.Failure, &failure)
}

func TestConnectorOutputLegacyEncodingAndCapacitySelection(t *testing.T) {
	response := connectorOutputFixedShape()
	response.Raw = []taskdir.LogDescriptor{{Path: "retained legacy descriptor"}}
	response.Continuation = &ContinuationResponse{ContinueCommand: "retained legacy command"}
	want := connectorOutputEncode(t, response)
	var stdout, stderr bytes.Buffer
	result := commandResult{response: response, code: 3}
	if code := presentCommandResult(Arguments{Command: response.Command, JSON: true, Connector: ConnectorArguments{IncludeWorkspaceConstraint: true}}, result, &stdout, &stderr); code != 3 || !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("legacy JSON or exit code changed: exit=%d %s", code, &stderr)
	}
	for _, connector := range []bool{false, true} {
		large := newResponse("status")
		large.Raw = []taskdir.LogDescriptor{{Path: strings.Repeat("x", task.MaxControlRecordSize)}}
		if connector {
			large.Connector = &ConnectorResponse{}
		}
		stdout.Reset()
		arguments := Arguments{Command: "status", JSON: true}
		if !connector {
			arguments.Connector.ExpectedRoot = strings.Repeat("a", 64)
		}
		if code := presentCommandResult(arguments, commandResult{response: large}, &stdout, &stderr); code != 0 || stdout.Len() <= task.MaxControlRecordSize {
			t.Fatal("guarded selector did not cover guards and Connector independently")
		}
	}
	ordinary := newResponse("logs")
	ordinary.Raw = []taskdir.LogDescriptor{{Path: strings.Repeat("x", task.MaxControlRecordSize)}}
	if err := writeJSON(&bytes.Buffer{}, ordinary); !errors.Is(err, ErrJSONResponseTooLarge) {
		t.Fatalf("ordinary 1R limit changed: %v", err)
	}
	schedule := newResponse("schedule")
	schedule.Schedule = &ScheduleResponse{Request: &schedulercontract.Request{Config: task.TaskConfig{Model: strings.Repeat("x", task.MaxControlRecordSize)}}}
	if err := writeJSON(&bytes.Buffer{}, schedule); err != nil {
		t.Fatalf("ordinary schedule 2R exception changed: %v", err)
	}
}

func TestConnectorOutputPolicyComesFromValidatedOriginAndSurvivesFailures(t *testing.T) {
	for _, command := range []string{"dispatch", "collect", "cancel", "resolve", "status", "result"} {
		t.Run(command, func(t *testing.T) {
			arguments := connectorOutputArguments(t, command, "bound")
			result := runCommandContext(t.Context(), arguments, Dependencies{Environment: []string{}})
			if result.connectorOutput != connectorOutputBinding || result.err == nil || result.code == 0 {
				t.Fatalf("validated origin policy was lost before a receipt: policy=%d err=%v code=%d", result.connectorOutput, result.err, result.code)
			}
			cleanupErr := errors.New("owned cleanup failed")
			mergeCommandClose(&result, func() error { return cleanupErr })
			if result.connectorOutput != connectorOutputBinding || result.code != 1 || !errors.Is(result.err, cleanupErr) {
				t.Fatal("cleanup replaced presentation policy or failure truth")
			}
		})
	}
	arguments := connectorOutputArguments(t, "resolve", "bound")
	if err := loadConnectorOrigin(t.Context(), &arguments, Dependencies{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(arguments.Connector.OriginFile); err != nil {
		t.Fatal(err)
	}
	if err := loadConnectorOrigin(t.Context(), &arguments, Dependencies{}); err != nil || arguments.Connector.outputPolicy != connectorOutputBinding {
		t.Fatalf("cached validated Origin was reread: %v", err)
	}
}

func TestConnectorOutputInvalidOriginAndReceiptDoNotSelectBindingProjection(t *testing.T) {
	arguments := connectorOutputArguments(t, "resolve", "bound")
	origin := task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "invalid operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64), Binding: "bound"}
	writeConnectorOutputJSON(t, arguments.Connector.OriginFile, origin)
	result := runCommandContext(t.Context(), arguments, Dependencies{Environment: []string{}})
	if result.err == nil || result.connectorOutput != connectorOutputLegacy {
		t.Fatal("an unvalidated Origin selected the binding projection")
	}
	result.response.Connector = &ConnectorResponse{Receipt: &taskdir.ConnectorReceipt{Origin: origin}}
	if commandJSONOutputPolicy(arguments, result).connector != connectorOutputLegacy {
		t.Fatal("presentation inferred binding mode from a receipt")
	}
}

func TestConnectorOutputContinuationEitherValidatedOriginSelectsProjection(t *testing.T) {
	for _, bindings := range [][2]string{{"", ""}, {"bound", ""}, {"", "bound"}, {"bound", "bound"}} {
		t.Run(bindings[0]+"/"+bindings[1], func(t *testing.T) {
			arguments := connectorOutputArguments(t, "continue", bindings[0])
			fresh := task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "successor", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("b", 64), Binding: bindings[1]}
			arguments.Connector.SuccessorOriginFile = filepath.Join(t.TempDir(), "successor.json")
			writeConnectorOutputJSON(t, arguments.Connector.SuccessorOriginFile, fresh)
			result := continueTaskContext(t.Context(), arguments, Dependencies{Environment: []string{}})
			want := connectorOutputLegacy
			if bindings[0] != "" || bindings[1] != "" {
				want = connectorOutputBinding
			}
			if result.connectorOutput != want || result.err == nil {
				t.Fatalf("two-Origin continuation selected policy=%d want=%d err=%v", result.connectorOutput, want, result.err)
			}
			for _, path := range []string{arguments.Connector.OriginFile, arguments.Connector.SuccessorOriginFile} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			if code := presentCommandResult(arguments, result, &output, io.Discard); code != result.code || output.Len() == 0 {
				t.Fatal("presentation reread Origins or changed the continuation failure")
			}
		})
	}
}

func TestConnectorOutputExpectedProjectUsesInputRecordBound(t *testing.T) {
	for _, size := range []int{task.MaxControlRecordSize, task.MaxControlRecordSize + 1} {
		_, err := ParseArguments([]string{"resolve", strings.Repeat("a", 32), "--expected-root", strings.Repeat("b", 64), "--expected-project", strings.Repeat("p", size), "--expected-origin", "unused.json"})
		if size == task.MaxControlRecordSize && err != nil {
			t.Fatalf("record-sized project was refused by an arbitrary smaller cap: %v", err)
		}
		if size > task.MaxControlRecordSize && !errors.Is(err, task.ErrControlRecordTooBig) {
			t.Fatalf("oversized project was not rejected at parse time: %v", err)
		}
	}
}

// This measures a populated conservative fixture, including mutually exclusive
// closure observations. Receipt strings share one task-record budget; the
// remaining request bytes receive the project-token base64 expansion. The
// measured remainder is evidence for this shape, not a universal exact maximum.
func TestConnectorOutputMeasuredEncoderRemainderAndExplicitOverflow(t *testing.T) {
	const record = task.MaxControlRecordSize
	response := connectorOutputFixedShape()
	response.Connector.ProjectToken = strings.Repeat("p", record)
	sharedReceiptBytes := connectorOutputReceiptStringBytes(t, response.Connector.Receipt)
	if sharedReceiptBytes >= record {
		t.Fatal("fixture exhausted the shared request-record budget before its project")
	}
	projectBytes := base64.RawURLEncoding.EncodedLen(record - sharedReceiptBytes)
	response.Connector.Receipt.ProjectToken = strings.Repeat("p", projectBytes)
	response.Outcome = connectorOutputBudgetOutcome(t, record)
	response.ResultRange.Data = bytes.Repeat([]byte{0xff}, int(resultread.MaxRangeBytes))
	data := connectorOutputEncode(t, projectConnectorOutput(response, connectorOutputBinding))
	// The outcome owner budget covers its value. Its key and separator
	// remain in the measured fixed remainder.
	outcomeBytes, err := json.Marshal(response.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomeBytes) != record-1 {
		t.Fatal("outcome variable budget was not exercised exactly")
	}
	ranges := 2 * base64.StdEncoding.EncodedLen(int(resultread.MaxRangeBytes))
	requestBytes := projectBytes + sharedReceiptBytes
	maxRequestBytes := base64.RawURLEncoding.EncodedLen(record)
	remainder := len(data) - requestBytes - record - len(outcomeBytes) - ranges
	availableRemainder := 4*record - maxRequestBytes - record - len(outcomeBytes) - ranges
	if requestBytes > maxRequestBytes {
		t.Fatal("receipt variables exceeded their shared expanded request budget")
	}
	if remainder > availableRemainder || len(data) > 4*record {
		t.Fatalf("populated conservative envelope exceeds budget: remainder=%d available=%d total=%d", remainder, availableRemainder, len(data))
	}
	t.Logf("measured encoder fixture: remainder=%d; receipt variables=%d (shared non-project=%d); complete envelope=%d; limit=%d; two base64 ranges=%d", remainder, requestBytes, sharedReceiptBytes, len(data), 4*record, ranges)
	response.Error = strings.Repeat("\x01", record)
	response.Stop.Message = strings.Repeat("\\\"", record)
	var output bytes.Buffer
	policy := jsonOutputPolicy{limit: 4 * record, connector: connectorOutputBinding}
	if err = writeJSONResponseWithPolicy(&output, response.Command, &response, nil, policy); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err = json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.ErrorTruncated || !decoded.Stop.MessageTruncated || !reflect.DeepEqual(decoded.Outcome, response.Outcome) || !reflect.DeepEqual(decoded.Connector.Receipt, response.Connector.Receipt) {
		t.Fatal("diagnostic bounding changed evidence or failed to report truncation")
	}
	response.Outcome.Usage[0].Model = new(strings.Repeat("m", 4*record))
	output.Reset()
	if err = writeJSONResponseWithPolicy(&output, response.Command, &response, nil, policy); !errors.Is(err, ErrJSONResponseTooLarge) || output.Len() != 0 {
		t.Fatalf("untrimmable overflow was not explicit and atomic: %v bytes=%d", err, output.Len())
	}
}

func connectorOutputReceiptStringBytes(t *testing.T, receipt *taskdir.ConnectorReceipt) int {
	t.Helper()
	options := receipt.ExecutionOptions
	values := []string{receipt.Origin.Binding, options.Provider, options.Permission, options.Model.Value, options.Effort.Value, receipt.WorkspaceConstraint.Token, receipt.WorkspaceConstraint.RelativeProject}
	var size int
	for _, value := range values {
		var output bytes.Buffer
		encoder := json.NewEncoder(&output)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
		// Contents are charged to the task-record variable budget. Quotes, keys
		// and separators remain in the measured presentation remainder.
		size += output.Len() - 3
	}
	return size
}

func connectorOutputBudgetOutcome(t *testing.T, budget int) *task.OutcomeRecord {
	t.Helper()
	id, digest, model := strings.Repeat("a", 32), strings.Repeat("b", 64), ""
	outcome := &task.OutcomeRecord{
		SchemaVersion: task.SchemaVersion, RootID: id, TaskID: id, SpecSHA256: digest, MetaSHA256: digest, EvidenceSHA256: digest,
		Verdict: task.VerdictCommitted, Predicate: task.FixturePredicateRef(), Payload: task.PayloadDescriptor{Basename: "result.txt", Length: math.MaxInt64, SHA256: digest},
		Usage: []task.UsageMetadata{{Scope: task.UsageScopeTask, Source: task.UsageSourceProviderEnvelope, Reliability: task.UsageReliabilityReported, Model: &model}},
	}
	data, err := task.MarshalCanonical(outcome)
	if err != nil {
		t.Fatal(err)
	}
	model = strings.Repeat("m", budget-len(data))
	if err = task.ValidateOutcomeRecord(outcome); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestConnectorOutputInventoryUsesLegacyProjectionAndGuardedCapacity(t *testing.T) {
	const record = task.MaxControlRecordSize
	root := task.RootRecord{SchemaVersion: task.SchemaVersion, RootID: strings.Repeat("a", 32), CreatedAt: "2026-11-21T00:00:00Z"}
	for range (record - 4096) / (len(task.NamespaceMutationFeature) + len(task.NamespaceAdmissionFeature) + 6) {
		root.RequiredFeatures = append(root.RequiredFeatures, task.NamespaceAdmissionFeature, task.NamespaceMutationFeature)
	}
	if err := task.ValidateRootRecord(&root); err != nil {
		t.Fatal(err)
	}
	before, err := task.MarshalCanonical(root)
	if err != nil {
		t.Fatal(err)
	}
	response := connectorOutputFixedShape()
	response.Command = "origin"
	response.Connector.Features = root.RequiredFeatures
	response.Connector.Receipt.Origin.Binding = strings.Repeat("b", record-4096)
	response.Connector.Inventory = &taskdir.OriginInventory{Entries: []taskdir.OriginInventoryEntry{{TaskID: response.TaskID, Resolution: "permanently_refused", Origin: response.Connector.Receipt.Origin.Clone(), Receipt: response.Connector.Receipt, SpecSHA256: strings.Repeat("a", 64), BriefSHA256: strings.Repeat("b", 64)}}, NextCursor: response.TaskID, MembershipSHA256: strings.Repeat("a", 64), CompletenessReceipt: strings.Repeat("b", 64)}
	// Inventory carries the receipt inside its one entry, never beside it.
	response.Connector.Receipt = nil
	want := connectorOutputEncode(t, response)
	var output bytes.Buffer
	arguments := Arguments{Command: "origin", JSON: true, Connector: ConnectorArguments{ExpectedRoot: strings.Repeat("a", 64), IncludeWorkspaceConstraint: true}}
	if code := presentCommandResult(arguments, commandResult{response: response}, &output, io.Discard); code != 0 || !bytes.Equal(output.Bytes(), want) || len(want) > 4*record {
		t.Fatalf("single-entry inventory changed or exceeded guarded capacity: code=%d bytes=%d", code, len(want))
	}
	after, err := task.MarshalCanonical(root)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inventory presentation changed the duplicate root features")
	}
	bound := projectConnectorOutput(response, connectorOutputBinding)
	if len(bound.Connector.Features) != 2 || len(root.RequiredFeatures) == 2 || !bytes.Equal(before, after) {
		t.Fatal("binding feature deduplication mutated the root record")
	}
	response.Connector.Inventory.Entries = append(response.Connector.Inventory.Entries, response.Connector.Inventory.Entries[0])
	output.Reset()
	policy := commandJSONOutputPolicy(arguments, commandResult{response: response})
	if err = writeJSONResponseWithPolicy(&output, "origin", &response, nil, policy); !errors.Is(err, ErrJSONResponseTooLarge) || output.Len() != 0 {
		t.Fatal("multi-entry inventory overflow skipped entries or hid failure")
	}
}

func connectorOutputArguments(t *testing.T, command, binding string) Arguments {
	t.Helper()
	origin := task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "prior", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64), Binding: binding}
	file := filepath.Join(t.TempDir(), "origin.json")
	writeConnectorOutputJSON(t, file, origin)
	return Arguments{Command: command, JSON: true, Root: filepath.Join(t.TempDir(), "missing"), TaskID: strings.Repeat("a", 32), SuccessorTaskID: strings.Repeat("b", 32), Connector: ConnectorArguments{
		ControlTimeout: time.Second, OriginFile: file, ExpectedRoot: strings.Repeat("a", 64), ExpectedProjectSHA256: strings.Repeat("b", 64), ExpectedSpecSHA256: strings.Repeat("c", 64),
	}}
}

func writeConnectorOutputJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := task.MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, path, data)
}

func connectorOutputEncode(t *testing.T, response Response) []byte {
	t.Helper()
	data, err := encodeJSONResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func connectorOutputFixedShape() Response {
	id, digest := strings.Repeat("a", 32), strings.Repeat("b", 64)
	features := []string{task.NamespaceAdmissionFeature, task.NamespaceMutationFeature, task.WorkspaceConstrainedConnectorFeature, task.PathProjectConnectorFeature, task.RootContextBindingFeature}
	registered, acknowledged, numeric := false, false, int64(math.MaxInt64)
	descriptor := task.PayloadDescriptor{Basename: "publish.reject", Length: math.MaxInt64, SHA256: digest}
	resultRange := &resultread.Range{Descriptor: descriptor, Offset: math.MaxInt64, NextOffset: math.MaxInt64}
	return Response{
		SchemaVersion: OutputSchemaVersion, Command: "continue", RootID: id, TaskID: id, TaskRecord: "not_created", Status: "interrupted", Admission: task.AdmissionNotAdmitted.String(), Liveness: task.LivenessUndetermined.String(), Publication: task.PublicationCommitted.String(),
		Connector: &ConnectorResponse{
			ExecutionAssuranceFeature: executionAssuranceFeature, ExecutionClosurePending: taskdir.ConnectorClosurePartialEvidence,
			ExecutionClosure:            &taskdir.ConnectorExecutionClosure{Kind: taskdir.ConnectorClosureUnregisteredRecovered, ReferenceSHA256: digest},
			ConnectorContextObservation: taskdir.ConnectorContextObservation{ContractVersion: 1, RootToken: digest, Features: append(features, features...)}, OriginRegistered: &registered,
			Resolution: "permanently_refused", CreationAttempt: "not_attempted", ResultRange: resultRange,
			OriginBarrier: &ConnectorOriginBarrierResponse{GatewayID: strings.Repeat("g", 128), RecoveryID: strings.Repeat("r", 128), Generation: strings.Repeat("n", 128)},
			Receipt: &taskdir.ConnectorReceipt{
				RootToken: digest, TaskID: id, SpecSHA256: digest, BriefSHA256: digest,
				Origin:              task.GatewayOrigin{GatewayID: strings.Repeat("g", 128), AdmissionGeneration: strings.Repeat("n", 128), OperationID: strings.Repeat("o", 128), PrincipalRef: strings.Repeat("p", 128), GatewayRequestSHA256: digest, Binding: "opaque-bound-origin"},
				WorkspaceConstraint: &task.WorkspaceConstraint{Token: "opaque-workspace-token", RelativeProject: "selected/project"},
				ExecutionOptions:    taskdir.ConnectorExecutionOptions{Provider: "antigravity:print", Permission: "workspace-write", BudgetNanoseconds: math.MaxInt64, Model: taskdir.ConnectorRequestedSelection{Kind: taskdir.ConnectorSelectionExplicit, Value: "explicit-model"}, Effort: taskdir.ConnectorRequestedSelection{Kind: taskdir.ConnectorSelectionExplicit, Value: "explicit-effort"}},
			},
		},
		ExecutionAssurance: &ExecutionAssuranceObservation{Feature: executionAssuranceFeature, State: ExecutionAssuranceRegistered, Backend: ExecutionBackendProcessGroup, Assurance: ExecutionAssuranceCooperative, RegistrationSHA256: digest},
		Payload:            &descriptor, ResultRange: resultRange, Stop: &StopResponse{RequestID: id, Cause: task.StopCauseOutputSilence, Action: "terminate", NumericTaskID: &numeric, Acknowledged: &acknowledged, ObservedState: "running", Message: "stop diagnostic", MessageTruncated: true},
		Failure: &DispatchFailureResponse{
			Code: "task_submission_preparation_failed", ReasonCode: string(tasklifecycle.ContinuationPredecessorRunnerLeaseObservationFailed), Stage: "resolve-continuation-supervisor", NextAction: "run_explicit_offline_cutover", RecoveryCommand: "unused human hint", RetryAfterSeconds: math.MaxInt,
			Inspection: &InspectionFailureResponse{Class: "supervisor_observation_failed", DeadlineState: "expired", JournalTaskID: id},
		}, Error: "diagnostic", ErrorTruncated: true,
	}
}
