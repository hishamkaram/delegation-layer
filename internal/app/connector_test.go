package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type connectorFixture struct {
	store                   *taskdir.Store
	task                    *taskdir.TaskDir
	request                 *task.TaskRecord
	origin                  task.GatewayOrigin
	rootToken, projectToken string
	deps                    Dependencies
}

func newConnectorFixture(t *testing.T) *connectorFixture {
	t.Helper()
	store, ordinary, _ := newAppTestTask(t, false)
	original, meta, err := ordinary.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnableConnectorContext(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	root, err := store.RootTokenContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "initial", "", "generation", root)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.OriginReopenContext(t.Context(), "gateway", "initial", "generation", inventory.CompletenessReceipt, root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := taskdir.ObserveProjectContext(t.Context(), original.CanonicalCwd)
	if err != nil {
		t.Fatal(err)
	}
	token, err := taskdir.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	origin := task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
	request := *original
	request.TaskID, request.Origin, request.ConnectorContext = strings.Repeat("d", 32), &origin, &task.ConnectorContext{RootToken: root, Project: project}
	proposed := *meta
	proposed.TaskID = request.TaskID
	td, err := store.CreateTaskContext(t.Context(), request.TaskID, &request, []byte("brief"), &proposed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return &connectorFixture{store: store, task: td, request: &request, origin: origin, rootToken: root, projectToken: token, deps: Dependencies{PredicateRegistry: predicate.Default, Environment: []string{}}}
}

func (f *connectorFixture) public(t *testing.T, args ...string) (Response, int) {
	t.Helper()
	data, err := task.MarshalCanonical(f.origin)
	if err != nil {
		t.Fatal(err)
	}
	deps := f.deps
	deps.Input = bytes.NewReader(data)
	args = append(args, "--root", f.store.Root, "--expected-root", f.rootToken, "--expected-project", f.projectToken, "--expected-origin", "-", "--json")
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, deps)
	var response Response
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("public response exit=%d stderr=%q output=%q: %v", code, stderr.String(), stdout.String(), err)
	}
	return response, code
}

func TestConnectorPublicPassiveResolveNeverSubmits(t *testing.T) {
	f := newConnectorFixture(t)
	response, code := f.public(t, "resolve", f.request.TaskID)
	if code != 0 || response.Connector == nil || response.Connector.Resolution != "existing" || response.Connector.Receipt == nil {
		t.Fatalf("resolve=%+v code=%d", response, code)
	}
	if response.Connector.Receipt.Origin != f.origin {
		t.Fatal("receipt changed immutable origin")
	}
	if _, err := f.task.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive resolve submitted: %v", err)
	}
	response, code = f.public(t, "status", f.request.TaskID)
	if code != 0 || response.Command != "status" {
		t.Fatalf("guarded status=%+v code=%d", response, code)
	}
	if _, err := f.task.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guarded status submitted: %v", err)
	}
}

func TestConnectorPublicMissingFenceIsPermanent(t *testing.T) {
	f := newConnectorFixture(t)
	missing := strings.Repeat("e", 32)
	response, code := f.public(t, "resolve", missing)
	if code != 0 || response.Connector.Resolution != "absent" {
		t.Fatalf("missing=%+v code=%d", response, code)
	}
	if refused, err := f.store.ReadNamespaceRefusalContext(t.Context(), missing); err != nil || refused != nil {
		t.Fatalf("passive absence fenced ID: %+v %v", refused, err)
	}
	response, code = f.public(t, "resolve", missing, "--fence-if-absent")
	if code != 0 || response.Connector.Resolution != "permanently_refused" {
		t.Fatalf("fence=%+v code=%d", response, code)
	}
	exists, err := f.store.TaskEntryExists(missing)
	if err != nil || exists {
		t.Fatalf("absent fence fabricated task: %v %v", exists, err)
	}
	response, code = f.public(t, "resolve", missing)
	if code != 0 || response.Connector.Resolution != "permanently_refused" {
		t.Fatalf("refusal lost: %+v code=%d", response, code)
	}
}

func TestConnectorPublicCancelReusesStableControlID(t *testing.T) {
	f := newConnectorFixture(t)
	control := strings.Repeat("f", 32)
	for range 2 {
		response, code := f.public(t, "cancel", f.request.TaskID, "--id", control)
		if code != 0 {
			t.Fatalf("cancel=%+v code=%d", response, code)
		}
		if response.Stop != nil && (response.Stop.RequestID != control || !response.Stop.Requested || !response.Stop.Terminated) {
			t.Fatalf("fresh cancellation did not prove closure: %+v", response.Stop)
		}

	}
	stops, err := f.task.ReadStopRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) != 1 || stops[0].Request == nil || stops[0].Request.RequestID != control {
		t.Fatalf("cancel replay created another control: %+v", stops)
	}
}

func TestConnectorPublicResumeWrongSpecCannotReachPreparation(t *testing.T) {
	f := newConnectorFixture(t)
	calls := 0
	f.deps.PrepareProfile = func(task.TaskRecord) (PreparedProfile, error) {
		calls++
		return PreparedProfile{}, errors.New("unexpected preparation")
	}
	response, code := f.public(t, "resolve", f.request.TaskID, "--resume-admission", "--expected-spec-sha256", strings.Repeat("b", 64))
	if code == 0 || calls != 0 {
		t.Fatalf("wrong spec reached recovery: code=%d calls=%d response=%+v", code, calls, response)
	}
	if _, err := f.task.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong spec submitted: %v", err)
	}
}

func TestConnectorPublicContextInspectDoesNotInitializeMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"context", "inspect", "--root", root, "--cwd", t.TempDir(), "--json"}, &stdout, &stderr, Dependencies{Environment: []string{}})
	if code == 0 {
		t.Fatalf("missing context succeeded: %s", stdout.String())
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection initialized missing root: %v", err)
	}
}

func TestConnectorPublicResultRejectsHiddenCorruption(t *testing.T) {
	f := newConnectorFixture(t)
	outcome := commitConnectorFixtureResult(t, f, "answer and hidden tail")
	originFile := filepath.Join(t.TempDir(), "origin.json")
	data, err := task.MarshalCanonical(f.origin)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, originFile, data)
	descriptor, err := task.MarshalCanonical(outcome.Payload)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"result", "read", f.request.TaskID, "--root", f.store.Root, "--expected-root", f.rootToken, "--expected-project", f.projectToken, "--expected-origin", originFile, "--expected-descriptor", "-", "--offset", "0", "--limit", "6", "--json"}
	run := func() (Response, int) {
		deps := f.deps
		deps.Input = bytes.NewReader(descriptor)
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr, deps)
		var response Response
		if readErr := json.Unmarshal(stdout.Bytes(), &response); readErr != nil {
			t.Fatalf("result output=%q stderr=%q: %v", stdout.String(), stderr.String(), readErr)
		}
		return response, code
	}
	response, code := run()
	if code != 0 || response.Connector.ResultRange == nil || string(response.Connector.ResultRange.Data) != "answer" {
		t.Fatalf("range=%+v code=%d", response, code)
	}
	payload, err := os.ReadFile(filepath.Join(f.task.Dir, outcome.Payload.Basename))
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 1
	writeAppTestFile(t, filepath.Join(f.task.Dir, outcome.Payload.Basename), payload)
	response, code = run()
	if code == 0 || response.Connector.ResultRange != nil {
		t.Fatalf("corrupt hidden tail released bytes: %+v code=%d", response, code)
	}
}

func commitConnectorFixtureResult(t *testing.T, f *connectorFixture, answer string) *task.OutcomeRecord {
	t.Helper()

	permit, err := f.task.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := permit.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = f.task.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = f.task.WriteRawFiles(answer, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = f.task.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	outcome, cleanupErr, err := f.task.Finalize(task.FixturePredicateRef())
	if err = errors.Join(err, cleanupErr, permit.Release()); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestConnectorPublicContextRequiresExplicitOriginRegistration(t *testing.T) {
	f := newConnectorFixture(t)
	inspect := func(gateway string) Response {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"context", "inspect", "--root", f.store.Root, "--cwd", f.request.CanonicalCwd, "--gateway-id", gateway, "--json"}, &stdout, &stderr, f.deps)
		var response Response
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if code != 0 {
			t.Fatalf("context exit=%d response=%+v stderr=%q", code, response, stderr.String())
		}
		return response
	}
	missing := inspect("unregistered")
	if missing.Connector.OriginRegistered == nil || *missing.Connector.OriginRegistered || missing.Connector.OriginBarrier != nil {
		t.Fatalf("missing origin invented generation: %+v", missing.Connector)
	}
	open := inspect("gateway")
	if open.Connector.OriginRegistered == nil || !*open.Connector.OriginRegistered || open.Connector.OriginBarrier == nil || !open.Connector.OriginBarrier.AdmissionOpen {
		t.Fatalf("open origin missing: %+v", open.Connector)
	}
	_, err := f.store.OriginBarrierContext(t.Context(), "gateway", "recovery-2", "generation", "generation-2", f.rootToken)
	if err != nil {
		t.Fatal(err)
	}
	closed := inspect("gateway")
	if closed.Connector.OriginBarrier == nil || closed.Connector.OriginBarrier.AdmissionOpen || closed.Connector.OriginBarrier.Generation != "generation-2" {
		t.Fatalf("closed origin hidden: %+v", closed.Connector)
	}
}

func TestConnectorWorkerStartRejectsProjectDirectorySubstitution(t *testing.T) {
	f := newConnectorFixture(t)
	original := f.request.CanonicalCwd
	retained := original + "-retained"
	if err := os.Rename(original, retained); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(original); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(retained, original); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	permit, err := f.task.PrepareStart(0)
	if permit != nil {
		t.Cleanup(func() {
			if releaseErr := permit.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		})
	}
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("substituted project obtained execution authority: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(f.task.Dir, "provider.start")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("substitution created start evidence: %v", err)
	}
}

func TestConnectorPublicResolveRecoversClosureAfterLostCancellationAck(t *testing.T) {
	f := newConnectorFixture(t)
	control := strings.Repeat("f", 32)
	first, code := f.public(t, "cancel", f.request.TaskID, "--id", control)
	if code != 0 || first.Stop == nil || !first.Stop.Terminated {
		t.Fatalf("initial cancellation did not close prepared task: %+v code=%d", first, code)
	}
	duplicate, code := f.public(t, "cancel", f.request.TaskID, "--id", control)
	if code != 0 || duplicate.Stop != nil {
		t.Fatalf("duplicate intent was not preserved: %+v code=%d", duplicate, code)
	}
	observed, code := f.public(t, "resolve", f.request.TaskID)
	if code != 0 || observed.Connector.ExecutionClosure == nil {
		t.Fatalf("lost cancellation ACK could not be resolved: %+v code=%d", observed, code)
	}
	request, err := f.task.ReadStopRequest(control)
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	closure := observed.Connector.ExecutionClosure
	if closure.Kind != taskdir.ConnectorClosureNeverStarted || closure.ReferenceSHA256 != task.ComputeSHA256(data) {
		t.Fatalf("closure references invented summary instead of stop owner evidence: %+v", closure)
	}
}

// Publication can precede the guardian's exact native worker Wait. Retrievable
// results and quota-releasing execution closure are independent observations.
func TestConnectorPublicAcceptedOutcomeDoesNotProveExecutionClosure(t *testing.T) {
	f := newConnectorFixture(t)
	outcome := commitConnectorFixtureResult(t, f, "accepted before worker lifetime closure")
	response, code := f.public(t, "resolve", f.request.TaskID)
	if code != 0 || response.Outcome == nil || response.Outcome.Verdict != outcome.Verdict {
		t.Fatalf("accepted outcome unavailable: code=%d response=%+v", code, response)
	}
	if response.Connector == nil || response.Connector.ExecutionClosure != nil || response.Connector.ExecutionClosurePending == "" {
		t.Fatalf("publication fabricated lifetime closure: %+v", response.Connector)
	}
}

func connectorProofFixtureResponse() Response {
	response := newResponse("resolve")
	registered := true
	response.Connector = &ConnectorResponse{
		ConnectorContextObservation: taskdir.ConnectorContextObservation{ContractVersion: 1, Features: []string{task.NamespaceAdmissionFeature}, RootToken: strings.Repeat("a", 64), ProjectToken: "fixture-context"},
		Resolution:                  "permanently_refused",
		ExecutionClosure:            &taskdir.ConnectorExecutionClosure{Kind: taskdir.ConnectorClosureNeverStarted, ReferenceSHA256: strings.Repeat("a", 64)},
		ExecutionClosurePending:     taskdir.ConnectorClosureOwnerBusy,
		OriginRegistered:            &registered,
		OriginBarrier:               &ConnectorOriginBarrierResponse{AdmissionOpen: true},
		Inventory:                   &taskdir.OriginInventory{Complete: true, CompletenessReceipt: "proof"},
	}
	response.Stop = &StopResponse{Terminated: true}
	return response
}

func assertConnectorFailureHasNoProof(t *testing.T, result commandResult) {
	t.Helper()
	connector := result.response.Connector
	if result.code == 0 || result.err == nil || connector == nil || connector.Resolution == "permanently_refused" || connector.ExecutionClosure != nil || connector.ExecutionClosurePending != "" || connector.Inventory != nil || connector.OriginBarrier != nil || connector.OriginRegistered != nil || len(connector.Features) != 0 || connector.RootToken != "" || connector.ProjectToken != "" || result.response.Stop.Terminated {
		t.Fatalf("failure retained readiness/release proof: %+v connector=%+v", result, connector)
	}
}

func TestConnectorFinalContextFailureScrubsReleaseProofs(t *testing.T) {
	f := newConnectorFixture(t)
	guard, err := f.store.RetainConnectorContext(t.Context(), f.rootToken, f.projectToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := commandResult{response: connectorProofFixtureResponse()}
	mergeConnectorClose(&result, func() error { return errors.Join(guard.Validate(ctx), guard.Close()) })
	assertConnectorFailureHasNoProof(t, result)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("lost validation error: %v", result.err)
	}
}

func TestConnectorDeferredStoreCloseFailureScrubsReleaseProofs(t *testing.T) {
	f := newConnectorFixture(t)
	owner, err := f.task.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	result := commandResult{response: connectorProofFixtureResponse()}
	closeObservationStore(&result, f.store)
	assertConnectorFailureHasNoProof(t, result)
}

func TestConnectorMutationFailureScrubsReleaseProofs(t *testing.T) {
	failure := errors.New("final mutation identity check failed")
	result := connectorFailure(connectorProofFixtureResponse(), failure)
	assertConnectorFailureHasNoProof(t, result)
	if !errors.Is(result.err, failure) {
		t.Fatal("mutation failure lost")
	}
}

func TestConnectorConcurrentPassiveResolveAndLocalCancelConverge(t *testing.T) {
	f := newConnectorFixture(t)
	owner, err := f.task.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	origin, err := task.MarshalCanonical(f.origin)
	if err != nil {
		t.Fatal(err)
	}
	type observed struct {
		output []byte
		code   int
	}
	start := make(chan struct{})
	results := make(chan observed, 2)
	control := strings.Repeat("b", 32)
	for _, argv := range [][]string{{"resolve", f.request.TaskID}, {"cancel", f.request.TaskID, "--id", control}} {
		go func(args []string) {
			<-start
			deps := f.deps
			deps.Input = bytes.NewReader(origin)
			args = append(args, "--root", f.store.Root, "--expected-root", f.rootToken, "--expected-project", f.projectToken, "--expected-origin", "-", "--json")
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr, deps)
			results <- observed{output: stdout.Bytes(), code: code}
		}(argv)
	}
	close(start)
	// Join both owned callers before any assertion can abort the test and start
	// fixture cleanup while another caller still uses its root.
	observations := []observed{<-results, <-results}
	for _, result := range observations {
		assertConcurrentConnectorResponse(t, result.output, result.code)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	// Retry the same control identity after owner contention, then passively recover
	// canonical closure. Neither observation nor replay may allocate another stop.
	f.public(t, "cancel", f.request.TaskID, "--id", control)
	response, code := f.public(t, "resolve", f.request.TaskID)
	if code != 0 || response.Connector.ExecutionClosure == nil {
		t.Fatalf("retry did not converge: code=%d response=%+v", code, response)
	}
	stops, err := f.task.ReadStopRecords()
	if err != nil || len(stops) != 1 || stops[0].Request.RequestID != control {
		t.Fatalf("concurrent replay duplicated stop: %+v %v", stops, err)
	}
}

func TestConnectorPublicReceiptExecutionOptionsMatchCanonicalRequest(t *testing.T) {
	f := newConnectorFixture(t)
	response, code := f.public(t, "resolve", f.request.TaskID)
	if code != 0 || response.Connector.Receipt == nil {
		t.Fatalf("receipt missing: code=%d response=%+v", code, response)
	}
	options := response.Connector.Receipt.ExecutionOptions
	if options.Provider != f.request.Provider || options.Permission != f.request.Mode || options.BudgetNanoseconds != f.request.BudgetNanos {
		t.Fatalf("receipt changed execution options: %+v", options)
	}
	for _, selection := range []struct {
		got       taskdir.ConnectorRequestedSelection
		requested string
	}{{options.Model, f.request.RequestedConfig.Model}, {options.Effort, f.request.RequestedConfig.Effort}} {
		if selection.requested == "" {
			if selection.got.Kind != taskdir.ConnectorSelectionNativeDefault || selection.got.Value != "" {
				t.Fatalf("native default falsely resolved: %+v", selection.got)
			}
		} else if selection.got.Kind != taskdir.ConnectorSelectionExplicit || selection.got.Value != selection.requested {
			t.Fatalf("requested selection changed: %+v", selection.got)
		}
	}
	data, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(f.request.CanonicalCwd)) || bytes.Contains(data, []byte("brief")) {
		t.Fatalf("receipt execution metadata leaked private content: %s", data)
	}
}

func assertConcurrentConnectorResponse(t *testing.T, output []byte, code int) {
	t.Helper()
	var response Response
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("invalid concurrent response %q: %v", output, err)
	}
	if code != 0 && code != 3 {
		t.Fatalf("concurrent control failed: code=%d response=%+v", code, response)
	}
	if response.Connector != nil && response.Connector.ExecutionClosure != nil {
		t.Fatal("busy lifetime yielded closure proof")
	}
}

func TestConnectorPublic64MiBResultVerifiesWithinDefaultDeadline(t *testing.T) {
	f := newConnectorFixture(t)
	const pattern = "0123456789abcdef"
	outcome := commitConnectorFixtureResult(t, f, strings.Repeat(pattern, 4<<20))
	descriptor, err := task.MarshalCanonical(outcome.Payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "descriptor.json")
	writeAppTestFile(t, path, descriptor)
	started := time.Now()
	response, code := f.public(t, "result", "read", f.request.TaskID, "--expected-descriptor", path, "--offset", "67108848", "--limit", "16")
	elapsed := time.Since(started)
	if code != 0 || response.Connector.ResultRange == nil {
		t.Fatalf("64MiB verification failed after %s: code=%d response=%+v", elapsed, code, response)
	}
	verified := response.Connector.ResultRange
	if string(verified.Data) != pattern || !verified.EOF || verified.NextOffset != 64<<20 || verified.Descriptor != outcome.Payload {
		t.Fatalf("64MiB range changed descriptor/bytes: %+v", verified)
	}
	t.Logf("public 64MiB whole-result verification and 16-byte range: %s (default control deadline %s)", elapsed, defaultConnectorControlTimeout)
}

func TestConnectorBusyLifetimeCleanupIsFatalNotPending(t *testing.T) {
	f := newConnectorFixture(t)
	lease := appCleanupLifetimeLease(t, f.store)
	f.store.SetFaultInjector(&joinedCleanupFault{name: ".lifetime-owner.lock", cause: task.ErrLockBusy})
	cause := lease.ReleaseWithError(nil)
	if cause == nil || !lease.CleanupPending() {
		t.Fatal("fixture did not retain lifetime cleanup")
	}
	result := connectorFailure(newResponse("resolve"), cause)
	if result.code != 1 || !errors.Is(result.err, task.ErrLockBusy) {
		t.Fatalf("active cleanup became retryable pending: %+v", result)
	}
	f.store.SetFaultInjector(nil)
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if taskdir.CleanupHasRetainedStore(cause) {
		t.Fatal("settled historical cleanup still owns dependency")
	}
	historical := connectorFailure(newResponse("resolve"), cause)
	if historical.code != 1 {
		t.Fatalf("historical cleanup fault became pending: %+v", historical)
	}
}

func TestConnectorJSONInputProcess(t *testing.T) {
	encoded := os.Getenv("DELEGATE_CONNECTOR_INPUT_FIXTURE")
	if encoded == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(encoded), &args); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	started := time.Now()
	code := Run(args, &output, &diagnostics, Dependencies{Input: os.Stdin, Environment: []string{}})
	if code == 0 || time.Since(started) > 2*time.Second {
		t.Fatalf("invalid/stalled input did not fail within its bound: code%d output%s diagnostics%s", code, output.String(), diagnostics.String())
	}
	if args[0] == "dispatch" && (code != 2 || !strings.Contains(diagnostics.String(), `unknown flag "--expected-spec-sha256"`)) {
		t.Fatalf("dispatch accepted unsupported spec flag: code%d diagnostics%s", code, diagnostics.String())
	}
	if _, err := os.Stdin.Stat(); err != nil {
		t.Fatalf("command closed borrowed stdin: %v", err)
	}
}

func TestConnectorJSONInputAndInvalidDispatchFailBeforePreparation(t *testing.T) {
	for _, name := range []string{"fifo", "stalled-stdin", "dispatch-spec-alone", "dispatch-spec-guarded"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "must-remain-absent")
			runConnectorJSONInputChild(t, connectorJSONProcessArguments(t, name, base, root))
			if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid input prepared root: %v", statErr)
			}
		})
	}
}

func connectorJSONProcessArguments(t *testing.T, name, base, root string) []string {
	t.Helper()
	if strings.HasPrefix(name, "dispatch-spec") {
		args := []string{"dispatch", "--root", root, "--expected-spec-sha256", strings.Repeat("a", 64), "--json"}
		if name == "dispatch-spec-guarded" {
			args = append(args, "--expected-root", "fixture-root", "--expected-project", "fixture-project", "--origin", "-")
		}
		return args
	}
	origin := "-"
	if name == "fifo" {
		origin = filepath.Join(base, "origin.fifo")
		if err := unix.Mkfifo(origin, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"resolve", strings.Repeat("a", 32), "--root", root, "--expected-root", "fixture-root", "--expected-project", "fixture-project", "--expected-origin", origin, "--control-timeout", "100ms", "--json"}
}

func runConnectorJSONInputChild(t *testing.T, args []string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := writer.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	child := exec.Command(executable, "-test.run=^TestConnectorJSONInputProcess$", "-test.count=1", "-test.timeout=5s")
	child.Env = append(os.Environ(), "DELEGATE_CONNECTOR_INPUT_FIXTURE="+string(encoded))
	child.Stdin = reader
	output, runErr := child.CombinedOutput()
	if child.ProcessState == nil || !child.ProcessState.Exited() || runErr != nil {
		t.Fatalf("bounded input child was not successfully joined: %v: %s", runErr, output)
	}
}

func TestConnectorExistingTaskSpecGuardsRejectBeforeEffects(t *testing.T) {
	for _, command := range []string{"status", "collect", "cancel", "result"} {
		t.Run(command, func(t *testing.T) {
			fixture := newConnectorFixture(t)
			args := []string{command, fixture.request.TaskID}
			if command == "cancel" {
				args = append(args, "--id", strings.Repeat("f", 32))
			}
			if command == "result" {
				args = []string{"result", "read", fixture.request.TaskID, "--expected-descriptor", filepath.Join(t.TempDir(), "must-not-read.json")}
			}
			args = append(args, "--expected-spec-sha256", strings.Repeat("b", 64))
			response, code := fixture.public(t, args...)
			if code != 1 || response.Outcome != nil {
				t.Fatalf("wrong existing spec was not rejected before work: code%d response%+v", code, response)
			}
			assertConnectorWrongSpecNoEffects(t, fixture, response)
		})
	}
}

func assertConnectorWrongSpecNoEffects(t *testing.T, fixture *connectorFixture, response Response) {
	t.Helper()
	if !strings.Contains(response.Error, task.ErrRequestConflict.Error()) {
		t.Fatalf("wrong spec reached another operation instead of its canonical guard: %s", response.Error)
	}
	if response.Connector != nil && (response.Connector.ResultRange != nil || response.Connector.ExecutionClosure != nil) {
		t.Fatal("wrong spec returned result bytes or release proof")
	}
	stops, err := fixture.task.ReadStopRecords()
	if err != nil || len(stops) != 0 {
		t.Fatalf("wrong spec created cancellation: %+v %v", stops, err)
	}
	if _, err = fixture.task.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong spec submitted work: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(fixture.task.Dir, "outcome.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong spec published outcome: %v", err)
	}
}

func TestConnectorPublicKnownSpecStatusMatchesCanonicalReceipt(t *testing.T) {
	fixture := newConnectorFixture(t)
	data, err := os.ReadFile(filepath.Join(fixture.task.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec := task.ComputeSHA256(data)
	response, code := fixture.public(t, "status", fixture.request.TaskID, "--expected-spec-sha256", spec)
	if code != 0 || response.Connector == nil || response.Connector.Receipt == nil || response.Connector.Receipt.SpecSHA256 != spec {
		t.Fatalf("matching known-spec status failed: code%d response%+v", code, response)
	}
}

func TestConnectorFenceRejectsWrongSpecBeforeEffects(t *testing.T) {
	for _, name := range []string{"complete", "partial"} {
		t.Run(name, func(t *testing.T) {
			f := newConnectorFixture(t)
			if name == "partial" {
				if err := os.Remove(filepath.Join(f.task.Dir, "meta.json")); err != nil {
					t.Fatal(err)
				}
			}
			response, code := f.public(t, "resolve", f.request.TaskID, "--fence-if-absent", "--expected-spec-sha256", strings.Repeat("b", 64))
			if code == 0 || !strings.Contains(response.Error, task.ErrRequestConflict.Error()) {
				t.Fatalf("wrong spec controlled task: %+v code=%d", response, code)
			}
			refusal, err := f.store.ReadNamespaceRefusalContext(t.Context(), f.request.TaskID)
			if err != nil || refusal != nil {
				t.Fatalf("wrong spec persisted refusal: %+v err=%v", refusal, err)
			}
		})
	}
}

func TestConnectorOperatorPartialFencePreservesRecordedAttribution(t *testing.T) {
	f := newConnectorFixture(t)
	if err := os.Remove(filepath.Join(f.task.Dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.FencePartialTaskContext(t.Context(), f.request.TaskID, nil, true)
	if err != nil || result.Resolution != "existing" {
		t.Fatalf("operator selection replaced known attribution: %+v err=%v", result, err)
	}
	refusal, err := f.store.ReadNamespaceRefusalContext(t.Context(), f.request.TaskID)
	if err != nil || refusal != nil {
		t.Fatalf("operator selection persisted unattributed refusal: %+v err=%v", refusal, err)
	}
}

// The project is an empty, fixture-owned directory. Restore its name before
// testing.T removes it; this also covers an early assertion or rename failure.
func moveConnectorFixtureProject(t *testing.T, f *connectorFixture, replace bool) {
	t.Helper()
	project := f.request.CanonicalCwd
	moved := project + ".moved"
	t.Cleanup(func() {
		if _, err := os.Stat(moved); errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			t.Error(err)
			return
		}
		if err := os.Remove(project); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
			return
		}
		if err := os.Rename(moved, project); err != nil {
			t.Error(err)
		}
	})
	if err := os.Rename(project, moved); err != nil {
		t.Fatal(err)
	}
	if replace {
		if err := os.Mkdir(project, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func forMovedConnectorProjects(t *testing.T, check func(*testing.T, *connectorFixture)) {
	t.Helper()
	for _, replace := range []bool{false, true} {
		name := "missing"
		if replace {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			f := newConnectorFixture(t)
			moveConnectorFixtureProject(t, f, replace)
			check(t, f)
		})
	}
}

func TestConnectorExistingControlsSurviveProjectMove(t *testing.T) {
	forMovedConnectorProjects(t, func(t *testing.T, f *connectorFixture) {
		for _, command := range []string{"resolve", "status"} {
			response, code := f.public(t, command, f.request.TaskID)
			if code != 0 || response.Connector == nil || response.Connector.Receipt == nil || response.Connector.Receipt.ProjectToken != f.projectToken {
				t.Fatalf("%s lost frozen task identity: %+v code=%d", command, response, code)
			}
		}
		cancelled, code := f.public(t, "cancel", f.request.TaskID, "--id", strings.Repeat("f", 32))
		if code != 0 || cancelled.Stop == nil || !cancelled.Stop.Requested {
			t.Fatalf("project move blocked canonical cancellation: %+v code=%d", cancelled, code)
		}
		resolved, code := f.public(t, "resolve", f.request.TaskID)
		if code != 0 || resolved.Connector == nil || resolved.Connector.ExecutionClosure == nil {
			t.Fatalf("project move blocked canonical closure: %+v code=%d", resolved, code)
		}
	})
}

func TestConnectorAbsentControlsSurviveProjectMove(t *testing.T) {
	forMovedConnectorProjects(t, func(t *testing.T, f *connectorFixture) {
		missing := strings.Repeat("e", 32)
		absent, code := f.public(t, "resolve", missing)
		if code != 0 || absent.Connector == nil || absent.Connector.Resolution != "absent" {
			t.Fatalf("project move blocked passive absence: %+v code=%d", absent, code)
		}
		fenced, code := f.public(t, "resolve", missing, "--fence-if-absent", "--expected-spec-sha256", strings.Repeat("b", 64))
		if code != 0 || fenced.Connector == nil || fenced.Connector.Resolution != "permanently_refused" {
			t.Fatalf("project move blocked absent fence: %+v code=%d", fenced, code)
		}
	})
}

func TestConnectorMovedProjectKeepsFrozenGuardsAndDeniesResume(t *testing.T) {
	forMovedConnectorProjects(t, func(t *testing.T, f *connectorFixture) {
		other, err := taskdir.ObserveProjectContext(t.Context(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		otherToken, err := taskdir.ProjectToken(other)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"root", "project", "origin", "spec"} {
			t.Run(field, func(t *testing.T) {
				assertMovedProjectGuardRejected(t, f, otherToken, field)
			})
		}
		assertMovedProjectCannotResume(t, f)
	})
}

func assertMovedProjectGuardRejected(t *testing.T, f *connectorFixture, otherToken, field string) {
	t.Helper()
	wrong := *f
	args := []string{"cancel", f.request.TaskID, "--id", strings.Repeat("f", 32)}
	switch field {
	case "root":
		wrong.rootToken = strings.Repeat("b", 64)
	case "project":
		wrong.projectToken = otherToken
	case "origin":
		wrong.origin.OperationID = "another-operation"
	case "spec":
		args = append(args, "--expected-spec-sha256", strings.Repeat("b", 64))
	}
	response, code := wrong.public(t, args...)
	if code == 0 || response.Outcome != nil || response.Stop != nil {
		t.Fatalf("moved project accepted wrong %s binding: code=%d response=%+v", field, code, response)
	}
}

func assertMovedProjectCannotResume(t *testing.T, f *connectorFixture) {
	t.Helper()
	_, _, spec, _, err := f.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	response, code := f.public(t, "resolve", f.request.TaskID, "--resume-admission", "--expected-spec-sha256", spec)
	if code == 0 || response.Outcome != nil {
		t.Fatalf("moved project resumed admission: code=%d response=%+v", code, response)
	}
	stops, err := f.task.ReadStopRecords()
	if err != nil || len(stops) != 0 {
		t.Fatalf("failed guards created cancellation: %+v %v", stops, err)
	}
	if _, err = f.task.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("moved project submitted work: %v", err)
	}
}

func TestConnectorAcceptedResultSurvivesProjectReplacement(t *testing.T) {
	f := newConnectorFixture(t)
	outcome := commitConnectorFixtureResult(t, f, "accepted answer")
	_, _, spec, _, err := f.task.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := task.MarshalCanonical(outcome.Payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "descriptor.json")
	writeAppTestFile(t, path, descriptor)
	moveConnectorFixtureProject(t, f, true)
	collected, code := f.public(t, "collect", f.request.TaskID, "--expected-spec-sha256", spec)
	if code != 0 || collected.Outcome == nil || collected.Outcome.Payload != outcome.Payload {
		t.Fatalf("replacement blocked accepted collection: %+v code=%d", collected, code)
	}
	response, code := f.public(t, "result", "read", f.request.TaskID, "--expected-spec-sha256", spec, "--expected-descriptor", path, "--limit", "64")
	if code != 0 || response.Connector == nil || response.Connector.ResultRange == nil || string(response.Connector.ResultRange.Data) != "accepted answer" {
		t.Fatalf("replacement blocked verified result: %+v code=%d", response, code)
	}
}

func TestConnectorRejectsIgnoredContextGuardFlags(t *testing.T) {
	for _, flag := range []string{"--expected-root", "--expected-project", "--expected-origin"} {
		_, err := ParseArguments([]string{"context", "inspect", flag, "ignored"})
		if err == nil || !strings.Contains(err.Error(), "context does not accept task") {
			t.Fatalf("context accepted ignored %s: %v", flag, err)
		}
	}
	for _, flag := range []string{"--expected-project", "--expected-origin"} {
		_, err := ParseArguments([]string{"origin", "inventory", "--gateway-id", "gateway", "--generation", "generation", "--expected-root", strings.Repeat("a", 64), flag, "ignored"})
		if err == nil || !strings.Contains(err.Error(), "origin does not accept task") {
			t.Fatalf("origin accepted ignored %s: %v", flag, err)
		}
	}
}

func TestConnectorDoesNotBroadenSchedulerRunContext(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunContext(t.Context(), []string{"status", strings.Repeat("a", 32), "--root", t.TempDir(), "--json"}, &stdout, &stderr, Dependencies{})
	if code != 2 || !strings.Contains(stderr.String(), "context execution requires a scheduler command") || stdout.Len() != 0 {
		t.Fatalf("scheduler context executed finite command: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
