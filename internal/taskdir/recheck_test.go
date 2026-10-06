package taskdir

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type recheckInterpreterFixture struct {
	ref         task.PredicateRef
	target      task.PredicateRef
	verdict     string
	answer      string
	diagnostics []task.OutputDiagnostic
	calls       atomic.Int32
	heldReader  io.Reader
	heldWriter  io.Writer
}

func (f *recheckInterpreterFixture) Reference() task.PredicateRef { return f.ref }
func (f *recheckInterpreterFixture) Target() task.PredicateRef    { return f.target }
func (f *recheckInterpreterFixture) CanRecover(source task.PredicateRef) bool {
	return source.Adapter == "fixture:test"
}

func (f *recheckInterpreterFixture) Evaluate(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "source refusal", Diagnostics: task.CloneOutputDiagnostics(f.diagnostics)}, nil
}

func (f *recheckInterpreterFixture) Recover(input predicate.Input, evidence predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	f.calls.Add(1)
	if !input.Seal.Predicate.Equal(f.target) && input.Seal.Predicate.Adapter != "fixture:test" {
		return task.Interpretation{}, task.ErrIdentityMismatch
	}
	var retained io.Reader
	if err := evidence.Read(predicate.Stdout, func(reader io.Reader) error {
		retained = reader
		_, err := io.Copy(io.Discard, reader)
		return err
	}); err != nil {
		return task.Interpretation{}, err
	}
	f.heldReader = retained
	f.heldWriter = out
	if f.verdict == task.VerdictRejected {
		return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "still rejected", Diagnostics: task.CloneOutputDiagnostics(f.diagnostics)}, nil
	}
	if _, err := io.WriteString(out, f.answer); err != nil {
		return task.Interpretation{}, err
	}
	return task.Interpretation{Verdict: task.VerdictCommitted, Diagnostics: task.CloneOutputDiagnostics(f.diagnostics)}, nil
}

func recheckTestRef(adapter, version string) task.PredicateRef {
	return task.PredicateRef{Adapter: adapter, Mode: "read-only", Version: version, SHA256: task.ComputeSHA256([]byte(adapter + "/" + version))}
}

func rejectedTaskForRecheck(t *testing.T, recovery *recheckInterpreterFixture) (*Store, *TaskDir, task.PredicateRef) {
	t.Helper()
	source := testInterpreterRef()
	sourceInterpreter := &recheckInterpreterFixture{ref: source, target: source, verdict: task.VerdictRejected, diagnostics: []task.OutputDiagnostic{{Code: "invalid_output", Message: "source output was not accepted", Field: "result"}}}
	registry, err := predicate.New(sourceInterpreter, recovery)
	must(t, err)
	store, err := InitStoreWithPredicates(filepath.Join(t.TempDir(), "state"), registry)
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	req, meta, brief := preparedInput(t, store)
	meta.Predicate = source
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	permit := consumeStart(t, td)
	must(t, td.RecordStarted(1))
	must(t, td.WriteRawFiles("source raw", "source stderr"))
	_, err = td.Seal(task.InvocationStarted, 0, "", source)
	must(t, err)
	must(t, permit.Release())
	outcome, cleanup, collectErr := td.Collect(source)
	must(t, cleanup)
	must(t, collectErr)
	if outcome == nil || outcome.Verdict != task.VerdictRejected {
		t.Fatalf("source outcome=%+v", outcome)
	}
	if len(outcome.Diagnostics) != 1 || outcome.Diagnostics[0].Code != "invalid_output" {
		t.Fatalf("source diagnostic was not published: %+v", outcome)
	}
	return store, td, source
}

func TestRecheckRecoversWithoutMutatingOriginalEvidenceAndCaches(t *testing.T) {
	target := recheckTestRef("codex:exec", "recovery-v1")
	recovery := &recheckInterpreterFixture{ref: target, target: target, verdict: task.VerdictCommitted, answer: "recovered answer"}
	_, td, source := rejectedTaskForRecheck(t, recovery)
	originalOutcome := readTestFile(t, filepath.Join(td.Dir, "outcome.json"))
	originalSeal := readTestFile(t, filepath.Join(td.Dir, "provider.exit"))
	originalRaw := readTestFile(t, filepath.Join(td.Dir, "raw", "stdout"))

	result, err := td.Recheck(recovery)
	must(t, err)
	if result.Report.Status != task.RecheckStatusRecovered || result.Report.TargetPredicate != target || result.AnswerPath == "" {
		t.Fatalf("unexpected recheck result: %+v", result)
	}
	if result.Report.SourcePredicate != source || result.Report.Result == nil {
		t.Fatalf("source/result descriptors missing: %+v", result.Report)
	}
	if string(readTestFile(t, filepath.Join(td.Dir, result.AnswerPath))) != "recovered answer" {
		t.Fatal("recovered answer was not durable")
	}
	if !bytes.Equal(originalOutcome, readTestFile(t, filepath.Join(td.Dir, "outcome.json"))) ||
		!bytes.Equal(originalSeal, readTestFile(t, filepath.Join(td.Dir, "provider.exit"))) ||
		!bytes.Equal(originalRaw, readTestFile(t, filepath.Join(td.Dir, "raw", "stdout"))) {
		t.Fatal("recheck mutated original evidence")
	}
	if recovery.calls.Load() != 1 {
		t.Fatalf("initial recovery calls=%d", recovery.calls.Load())
	}
	second, err := td.Recheck(recovery)
	must(t, err)
	if second.AssessmentID != result.AssessmentID || recovery.calls.Load() != 1 {
		t.Fatalf("cached recovery was re-evaluated: first=%+v second=%+v calls=%d", result, second, recovery.calls.Load())
	}
	if _, cleanupErr, collectErr := td.Collect(source); cleanupErr != nil || collectErr != nil {
		err = errors.Join(cleanupErr, collectErr)
		t.Fatalf("original rejection became unreadable: %v", err)
	}
	assertRecoveryCapabilitiesExpired(t, recovery)
}

func assertRecoveryCapabilitiesExpired(t *testing.T, recovery *recheckInterpreterFixture) {
	t.Helper()
	if recovery.heldReader == nil || recovery.heldWriter == nil {
		t.Fatal("recovery capabilities were not retained for lifetime test")
	}
	if _, err := recovery.heldReader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retained evidence reader remained usable: %v", err)
	}
	if _, err := recovery.heldWriter.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retained recovery writer remained usable: %v", err)
	}
}

func TestRecheckRetriesAfterPayloadAndReportDurabilityUncertainty(t *testing.T) {
	for _, target := range []string{recheckResultName, recheckReportName} {
		t.Run(target, func(t *testing.T) {
			ref := recheckTestRef("codex:exec", "fault-retry-v1")
			recovery := &recheckInterpreterFixture{ref: ref, target: ref, verdict: task.VerdictCommitted, answer: "retry answer"}
			store, td, _ := rejectedTaskForRecheck(t, recovery)
			store.SetFaultInjector(&orderedFault{target: target, fail: "post-link"})
			if _, err := td.Recheck(recovery); !errors.Is(err, task.ErrUncertainDurability) {
				t.Fatalf("first recheck error=%v", err)
			}
			store.SetFaultInjector(nil)
			result, err := td.Recheck(recovery)
			must(t, err)
			if result.Report.Status != task.RecheckStatusRecovered || result.AnswerPath == "" {
				t.Fatalf("retry did not recover: %+v", result)
			}
			wantCalls := int32(1)
			if target == recheckResultName {
				wantCalls = 2
			}
			if got := recovery.calls.Load(); got != wantCalls {
				t.Fatalf("recovery calls=%d, want %d", got, wantCalls)
			}
		})
	}
}

func TestConcurrentEqualRechecksConvergeToOneReport(t *testing.T) {
	ref := recheckTestRef("codex:exec", "concurrent-v1")
	recovery := &recheckInterpreterFixture{ref: ref, target: ref, verdict: task.VerdictCommitted, answer: "concurrent answer"}
	store, td, _ := rejectedTaskForRecheck(t, recovery)
	other, err := store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })

	start := make(chan struct{})
	results := make(chan struct {
		result *RecheckResult
		err    error
	}, 2)
	var group sync.WaitGroup
	for _, handle := range []*TaskDir{td, other} {
		group.Add(1)
		go func(handle *TaskDir) {
			defer group.Done()
			<-start
			result, err := handle.Recheck(recovery)
			results <- struct {
				result *RecheckResult
				err    error
			}{result, err}
		}(handle)
	}
	close(start)
	group.Wait()
	close(results)
	var assessmentID string
	for result := range results {
		must(t, result.err)
		if result.result == nil || result.result.Report == nil {
			t.Fatal("concurrent recheck returned no report")
		}
		if assessmentID == "" {
			assessmentID = result.result.AssessmentID
		} else if result.result.AssessmentID != assessmentID {
			t.Fatalf("concurrent assessments diverged: %s and %s", assessmentID, result.result.AssessmentID)
		}
	}
	if got := recovery.calls.Load(); got != 1 {
		t.Fatalf("concurrent requests evaluated recovery %d times", got)
	}
}

func TestRejectedRecheckStoresDiagnosticWithoutAnswer(t *testing.T) {
	target := recheckTestRef("claude:print", "recovery-refusal-v1")
	diagnostic := task.OutputDiagnostic{Code: "incomplete_output", Message: "required terminal record was not observed", Field: "terminal"}
	recovery := &recheckInterpreterFixture{ref: target, target: target, verdict: task.VerdictRejected, diagnostics: []task.OutputDiagnostic{diagnostic}}
	_, td, _ := rejectedTaskForRecheck(t, recovery)
	result, err := td.Recheck(recovery)
	must(t, err)
	if result.Report.Status != task.RecheckStatusRejected || result.Report.Result != nil || result.AnswerPath != "" {
		t.Fatalf("rejected recheck retained an answer: %+v", result)
	}
	if len(result.Report.Diagnostics) != 1 || result.Report.Diagnostics[0] != diagnostic {
		t.Fatalf("diagnostic was not retained: %+v", result.Report.Diagnostics)
	}
	if _, err = os.Lstat(filepath.Join(td.Dir, "rechecks", result.AssessmentID, recheckResultName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected recheck created result: %v", err)
	}
}

func TestRecheckRejectsCorruptCachedAnswer(t *testing.T) {
	target := recheckTestRef("codex:exec", "corrupt-cache-v1")
	recovery := &recheckInterpreterFixture{ref: target, target: target, verdict: task.VerdictCommitted, answer: "stable"}
	_, td, _ := rejectedTaskForRecheck(t, recovery)
	result, err := td.Recheck(recovery)
	must(t, err)
	writeTestFile(t, filepath.Join(td.Dir, result.AnswerPath), []byte("tampered"))
	if _, err = td.Recheck(recovery); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("corrupt cached answer accepted: %v", err)
	}
	if recovery.calls.Load() != 1 {
		t.Fatalf("corrupt cache triggered a new recovery call: %d", recovery.calls.Load())
	}
}

func TestRecheckRejectsCorruptSourceEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		path   string
		data   []byte
		wantIn error
	}{
		{name: "seal", path: "provider.exit", data: []byte("not-json"), wantIn: task.ErrEvidenceFault},
		{name: "raw", path: filepath.Join("raw", "stdout"), data: []byte("changed"), wantIn: task.ErrEvidenceFault},
		{name: "payload", path: "publish.reject", data: []byte("changed"), wantIn: task.ErrEvidenceFault},
		{name: "outcome", path: "outcome.json", data: []byte("not-json"), wantIn: task.ErrInvariantFault},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := recheckTestRef("codex:exec", "corrupt-source-"+test.name)
			recovery := &recheckInterpreterFixture{ref: target, target: target, verdict: task.VerdictCommitted, answer: "must not run"}
			_, td, _ := rejectedTaskForRecheck(t, recovery)
			writeTestFile(t, filepath.Join(td.Dir, test.path), test.data)
			if _, err := td.Recheck(recovery); !errors.Is(err, test.wantIn) {
				t.Fatalf("corrupt %s returned %v, want %v", test.name, err, test.wantIn)
			}
			if recovery.calls.Load() != 0 {
				t.Fatalf("corrupt %s invoked recovery", test.name)
			}
		})
	}
}

func TestRecheckRejectsUnsupportedCachedReportRevision(t *testing.T) {
	target := recheckTestRef("codex:exec", "unsupported-report-v1")
	recovery := &recheckInterpreterFixture{ref: target, target: target, verdict: task.VerdictCommitted, answer: "stable"}
	_, td, _ := rejectedTaskForRecheck(t, recovery)
	result, err := td.Recheck(recovery)
	must(t, err)
	reportData := readTestFile(t, filepath.Join(td.Dir, result.ReportPath))
	var report task.RecheckReport
	must(t, task.DecodeStrict(reportData, &report))
	report.SchemaVersion++
	reportData, err = task.MarshalCanonical(&report)
	must(t, err)
	writeTestFile(t, filepath.Join(td.Dir, result.ReportPath), reportData)
	if _, err = td.Recheck(recovery); !errors.Is(err, task.ErrUnsupportedSchema) {
		t.Fatalf("unsupported cached revision accepted: %v", err)
	}
	if recovery.calls.Load() != 1 {
		t.Fatalf("unsupported cached revision invoked recovery: %d", recovery.calls.Load())
	}
}

func TestCachedRecheckRevalidatesOriginalRawEvidence(t *testing.T) {
	for _, path := range []string{"raw/stdout", "raw/stderr"} {
		t.Run(path, func(t *testing.T) {
			target := recheckTestRef("codex:exec", "cached-source-v1")
			recovery := &recheckInterpreterFixture{ref: target, target: target, verdict: task.VerdictCommitted, answer: "stable"}
			_, td, _ := rejectedTaskForRecheck(t, recovery)
			_, err := td.Recheck(recovery)
			must(t, err)
			writeTestFile(t, filepath.Join(td.Dir, path), []byte("changed source"))
			if _, err = td.Recheck(recovery); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("cached recovery accepted corrupt source: %v", err)
			}
			if available, availableErr := td.RecheckAvailable(recovery); available || !errors.Is(availableErr, task.ErrEvidenceFault) {
				t.Fatalf("corrupt recovery availability: %v %v", available, availableErr)
			}
			if recovery.calls.Load() != 1 {
				t.Fatal("corrupt source was reinterpreted")
			}
		})
	}
}

func TestCachedRecheckRepeatsReportDurabilityBarrier(t *testing.T) {
	ref := recheckTestRef("codex:exec", "cached-barrier-v1")
	recovery := &recheckInterpreterFixture{ref: ref, target: ref, verdict: task.VerdictCommitted, answer: "answer"}
	store, td, _ := rejectedTaskForRecheck(t, recovery)
	store.SetFaultInjector(&orderedFault{target: recheckReportName, fail: "post-link"})
	for attempt := range 2 {
		if _, err := td.Recheck(recovery); !errors.Is(err, task.ErrUncertainDurability) {
			t.Fatalf("attempt %d skipped report durability barrier: %v", attempt, err)
		}
	}
	store.SetFaultInjector(nil)
	_, err := td.Recheck(recovery)
	must(t, err)
	if recovery.calls.Load() != 1 {
		t.Fatal("visible report was reinterpreted")
	}
}
