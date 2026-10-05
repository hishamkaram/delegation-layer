package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type appRecoveryInterpreter struct {
	ref         task.PredicateRef
	calls       atomic.Int32
	answer      string
	diagnostics []task.OutputDiagnostic
}

func (i *appRecoveryInterpreter) Reference() task.PredicateRef { return i.ref }
func (i *appRecoveryInterpreter) Target() task.PredicateRef    { return i.ref }
func (i *appRecoveryInterpreter) CanRecover(source task.PredicateRef) bool {
	return source.Equal(task.FixturePredicateRef())
}

func (i *appRecoveryInterpreter) Evaluate(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "source refusal", Diagnostics: task.CloneOutputDiagnostics(i.diagnostics)}, nil
}

func (i *appRecoveryInterpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	i.calls.Add(1)
	if !input.Seal.Predicate.Equal(task.FixturePredicateRef()) {
		return task.Interpretation{}, task.ErrIdentityMismatch
	}
	if err := raw.Read(predicate.Stdout, func(reader io.Reader) error { _, err := io.Copy(io.Discard, reader); return err }); err != nil {
		return task.Interpretation{}, err
	}
	if _, err := io.WriteString(out, i.answer); err != nil {
		return task.Interpretation{}, err
	}
	return task.Interpretation{Verdict: task.VerdictCommitted}, nil
}

func TestRecheckCommandIsOfflineAndRetainsRejectedPublication(t *testing.T) {
	target := task.PredicateRef{Adapter: "codex:exec", Mode: "read-only", Version: "app-recovery-v1", SHA256: task.ComputeSHA256([]byte("app-recovery-v1"))}
	recovery := &appRecoveryInterpreter{ref: target, answer: "offline answer"}
	diagnostic := task.OutputDiagnostic{Code: "incomplete_output", Message: "provider output did not contain a complete answer", Field: "result"}
	sourceInterpreter := &appRecoveryInterpreter{ref: task.FixturePredicateRef(), diagnostics: []task.OutputDiagnostic{diagnostic}}
	registry, err := predicate.New(sourceInterpreter, recovery)
	if err != nil {
		t.Fatal(err)
	}
	root, taskID := appRejectedRecoveryTask(t, registry)

	assertRecoveryOffered(t, registry, root, taskID, diagnostic)

	response := runRecheckTestCommand(t, registry, root, taskID, "recheck", 0)
	assertRecoveredResponse(t, response, target, recovery.answer)
	if recovery.calls.Load() != 1 {
		t.Fatalf("unexpected recovery calls=%d", recovery.calls.Load())
	}
	runRecheckTestCommand(t, registry, root, taskID, "recheck", 0)
	if recovery.calls.Load() != 1 {
		t.Fatalf("cached recheck invoked interpreter: %d", recovery.calls.Load())
	}
	finalCollect := runRecheckTestCommand(t, registry, root, taskID, "collect", 4)
	if finalCollect.Failure == nil || finalCollect.Failure.NextAction != "stop_and_report" {
		t.Fatalf("assessed rejected collect still advertised recheck: %+v", finalCollect)
	}
}

func assertRecoveredResponse(t *testing.T, response Response, target task.PredicateRef, wantAnswer string) {
	t.Helper()
	if response.Publication != task.PublicationRejected.String() || response.Outcome == nil || response.Recheck == nil || response.Recheck.Status != task.RecheckStatusRecovered {
		t.Fatalf("original rejection or recovery missing: %+v", response)
	}
	if response.Recheck.Answer == nil || !filepath.IsAbs(response.Recheck.AnswerPath) || !filepath.IsAbs(response.Recheck.ReportPath) || response.Recheck.Target != target {
		t.Fatalf("recovery answer/target missing: %+v", response.Recheck)
	}
	if response.Error != "" || response.Failure != nil {
		t.Fatalf("recovered response retained original error: %+v", response)
	}
	answer, err := os.ReadFile(response.Recheck.AnswerPath)
	if err != nil || string(answer) != wantAnswer {
		t.Fatalf("public answer path unreadable: %q %v", answer, err)
	}
}

func assertRecoveryOffered(t *testing.T, registry predicate.Registry, root, taskID string, diagnostic task.OutputDiagnostic) {
	t.Helper()
	collectResponse := runRecheckTestCommand(t, registry, root, taskID, "collect", 4)
	if collectResponse.Publication != task.PublicationRejected.String() || collectResponse.Failure == nil || collectResponse.Failure.NextAction != "recheck" {
		t.Fatalf("eligible rejected collect did not advertise recheck: %+v", collectResponse)
	}
	if len(collectResponse.Diagnostics) != 1 || collectResponse.Diagnostics[0] != diagnostic || collectResponse.Error != diagnostic.Code+": "+diagnostic.Message {
		t.Fatalf("rejection diagnostic was not exposed safely: %+v", collectResponse)
	}
	statusResponse := runRecheckTestCommand(t, registry, root, taskID, "status", 0)
	if len(statusResponse.Diagnostics) != 1 || statusResponse.Error != diagnostic.Code+": "+diagnostic.Message {
		t.Fatalf("status omitted rejection diagnostic: %+v", statusResponse)
	}
}

func runRecheckTestCommand(t *testing.T, registry predicate.Registry, root, taskID, command string, wantCode int) Response {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--root", root, command, taskID, "--json"}, &stdout, &stderr, Dependencies{PredicateRegistry: func() predicate.Registry { return registry }})
	if code != wantCode || stderr.Len() != 0 {
		t.Fatalf("%s code=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func appRejectedRecoveryTask(t *testing.T, registry predicate.Registry) (string, string) {
	t.Helper()
	store, err := taskdir.InitStoreWithPredicates(filepath.Join(t.TempDir(), "state"), registry)
	if err != nil {
		t.Fatal(err)
	}
	taskID := strings.Repeat("e", 32)
	td, _ := newAppTaskInStore(t, store, taskID, false, nil)
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles("source", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("source collection cleanup=%v err=%v", cleanupErr, collectErr)
	}
	root := store.Root
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	return root, taskID
}

func TestSuccessfulOutputDiagnosticsRemainWarnings(t *testing.T) {
	warning := task.OutputDiagnostic{Code: "optional_usage_unavailable", Message: "optional usage was unavailable"}
	response := newResponse("collect")
	result := collectionResult(response, &task.OutcomeRecord{Verdict: task.VerdictCommitted, Diagnostics: []task.OutputDiagnostic{warning}}, nil)
	if result.code != 0 || result.response.Error != "" || len(result.response.Diagnostics) != 1 {
		t.Fatalf("committed warning became failure: %+v", result)
	}
	response.Outcome = &task.OutcomeRecord{Verdict: task.VerdictRejected}
	response.Recheck = &RecheckResponse{Status: task.RecheckStatusRecovered}
	applyOutputDiagnostics(&response, []task.OutputDiagnostic{warning})
	if response.Error != "" || len(response.Diagnostics) != 1 {
		t.Fatalf("recovered warning became failure: %+v", response)
	}
}
