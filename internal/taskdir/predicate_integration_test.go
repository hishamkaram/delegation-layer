package taskdir

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type interpreterFixture struct {
	ref      task.PredicateRef
	evaluate func(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error)
}

func (f interpreterFixture) Reference() task.PredicateRef { return f.ref }
func (f interpreterFixture) Evaluate(input predicate.Input, evidence predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return f.evaluate(input, evidence, out)
}

func testInterpreterRef() task.PredicateRef {
	r := task.FixturePredicateRef()
	r.Version = "storage-test-v2"
	r.SHA256 = task.ComputeSHA256([]byte("storage tests only"))
	return r
}

func streamInterpreter(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	err := raw.Read(predicate.Stdout, func(r io.Reader) error { _, err := io.Copy(out, r); return err })
	return task.Interpretation{Verdict: task.VerdictCommitted}, err
}

func interpreterTask(t *testing.T, fn func(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error), raw string) (*Store, *TaskDir) {
	t.Helper()
	ref := testInterpreterRef()
	registry, err := predicate.New(predicate.FixtureV1(), interpreterFixture{ref: ref, evaluate: fn})
	must(t, err)
	s, err := InitStoreWithPredicates(filepath.Join(t.TempDir(), "state"), registry)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	req, meta, brief := preparedInput(t, s)
	meta.Predicate = ref
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	permit := consumeStart(t, td)
	must(t, td.WriteRawFiles(raw, "stderr tail"))
	_, err = td.Seal(task.InvocationStarted, 0, "", ref)
	must(t, err)
	must(t, permit.Release())
	return s, td
}

func collectInterpreter(t *testing.T, td *TaskDir) *task.OutcomeRecord {
	t.Helper()
	out, cleanup, err := td.Collect(testInterpreterRef())
	must(t, err)
	must(t, cleanup)
	if out == nil {
		t.Fatal("missing outcome")
	}
	return out
}

func TestRegisteredInterpretationStreamsBeyondControlLimit(t *testing.T) {
	answer := strings.Repeat("large-answer\n", 100000)
	_, td := interpreterTask(t, streamInterpreter, answer)
	out := collectInterpreter(t, td)
	if out.Payload.Length != int64(len(answer)) || out.Payload.SHA256 != task.ComputeSHA256([]byte(answer)) {
		t.Fatal("streamed answer changed")
	}
	if !bytes.Equal(readTestFile(t, filepath.Join(td.Dir, "result.txt")), []byte(answer)) {
		t.Fatal("payload changed")
	}
}

func TestUnknownInterpreterCannotDemoteWinner(t *testing.T) {
	s, td := interpreterTask(t, streamInterpreter, "answer")
	winner := collectInterpreter(t, td)
	// Side metadata is optional after a valid terminal outcome.
	writeTestFile(t, filepath.Join(td.Dir, "provider.ref.json"), []byte("malformed"))
	empty, err := predicate.New()
	must(t, err)
	reopened, err := OpenStoreWithPredicates(s.Root, empty)
	must(t, err)
	defer func() { must(t, reopened.Close()) }()
	fresh, err := reopened.OpenTask(td.TaskID)
	must(t, err)
	defer func() { must(t, fresh.Close()) }()
	out, cleanup, err := fresh.Collect(testInterpreterRef())
	must(t, err)
	must(t, cleanup)
	if !task.CompareOutcomes(winner, out) {
		t.Fatal("missing interpreter demoted winner")
	}
}

func TestUnknownInterpreterDoesNotStageCandidate(t *testing.T) {
	s, td := interpreterTask(t, streamInterpreter, "answer")
	empty, err := predicate.New()
	must(t, err)
	reopened, err := OpenStoreWithPredicates(s.Root, empty)
	must(t, err)
	defer func() { must(t, reopened.Close()) }()
	fresh, err := reopened.OpenTask(td.TaskID)
	must(t, err)
	defer func() { must(t, fresh.Close()) }()
	out, cleanup, err := fresh.Collect(testInterpreterRef())
	must(t, cleanup)
	if out != nil || !errors.Is(err, task.ErrIncompatiblePredicate) {
		t.Fatalf("unknown reference result: %v %v", out, err)
	}
	testAbsent(t, filepath.Join(td.Dir, "result.txt"))
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
	entries, err := os.ReadDir(td.Dir)
	must(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "stage.") {
			t.Fatal("unknown interpreter created stage")
		}
	}
}

func TestSemanticRejectionDiscardsAnswerPrefix(t *testing.T) {
	fn := func(_ predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		if _, err := io.WriteString(out, "not final"); err != nil {
			return task.Interpretation{}, err
		}
		// A parser can convert its callback's semantic syntax error into its contract
		// refusal. Storage must retain only actual stream I/O faults as fatal.
		err := raw.Read(predicate.Stdout, func(io.Reader) error { return errors.New("malformed provider syntax") })
		if err == nil {
			return task.Interpretation{}, errors.New("test callback did not run")
		}
		return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "invalid_output"}, nil
	}
	_, td := interpreterTask(t, fn, "malformed")
	out := collectInterpreter(t, td)
	if out.Verdict != task.VerdictRejected || string(readTestFile(t, filepath.Join(td.Dir, "publish.reject"))) != "invalid_output" {
		t.Fatal("refusal did not replace private answer prefix")
	}
	testAbsent(t, filepath.Join(td.Dir, "result.txt"))
}

func TestInterpreterCannotRetainReaderOrWriter(t *testing.T) {
	var heldReader io.Reader
	var heldWriter io.Writer
	fn := func(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		heldWriter = out
		err := raw.Read(predicate.Stdout, func(r io.Reader) error { heldReader = r; _, e := io.Copy(out, r); return e })
		return task.Interpretation{Verdict: task.VerdictCommitted}, err
	}
	_, td := interpreterTask(t, fn, "answer")
	collectInterpreter(t, td)
	if _, err := heldReader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("reader escaped callback: %v", err)
	}
	if _, err := heldWriter.Write([]byte("later")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("writer escaped evaluation: %v", err)
	}
}

func TestEvidenceCallbacksAllowNestedAndSequentialReads(t *testing.T) {
	var held io.Reader
	fn := func(_ predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		var first, nested, sequential []byte
		err := raw.Read(predicate.Stdout, func(outer io.Reader) error {
			held = outer
			var err error
			first, err = io.ReadAll(outer)
			if err != nil {
				return err
			}
			return raw.Read(predicate.Stdout, func(inner io.Reader) error {
				var err error
				nested, err = io.ReadAll(inner)
				return err
			})
		})
		if err != nil {
			return task.Interpretation{}, err
		}
		if err = raw.Read(predicate.Stdout, func(reader io.Reader) error {
			var readErr error
			sequential, readErr = io.ReadAll(reader)
			return readErr
		}); err != nil {
			return task.Interpretation{}, err
		}
		if !bytes.Equal(first, nested) || !bytes.Equal(first, sequential) {
			return task.Interpretation{}, errors.New("nested or sequential evidence read changed bytes")
		}
		_, err = out.Write(first)
		return task.Interpretation{Verdict: task.VerdictCommitted}, err
	}
	_, td := interpreterTask(t, fn, "nested evidence")
	collectInterpreter(t, td)
	if _, err := held.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("callback reader remained usable: %v", err)
	}
}

func TestPayloadWriterExpiresAtEvaluationReturn(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	stage, err := td.newPayloadStage("result.txt")
	must(t, err)
	t.Cleanup(func() { must(t, stage.discard()) })
	stage.expire()
	if _, err = stage.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("expired writer accepted bytes: %v", err)
	}
}

func TestInterpreterCannotIgnorePayloadWriteFailure(t *testing.T) {
	fn := func(_ predicate.Input, _ predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		if _, err := out.Write([]byte("prefix")); err == nil {
			return task.Interpretation{}, errors.New("expected injected write failure")
		}
		return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "semantic_refusal"}, nil
	}
	s, td := interpreterTask(t, fn, "answer")
	s.SetFaultInjector(&orderedFault{target: "result.txt", fail: "write"})
	out, cleanup, err := td.Collect(testInterpreterRef())
	must(t, cleanup)
	if err == nil || out != nil {
		t.Fatal("ignored stage failure published")
	}
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
	testAbsent(t, filepath.Join(td.Dir, "publish.reject"))
}

func TestHeldEvidenceFailureCannotBeConvertedIntoRefusal(t *testing.T) {
	fn := func(_ predicate.Input, raw predicate.Evidence, _ io.Writer) (task.Interpretation, error) {
		held, ok := raw.(*sealedEvidence)
		if !ok {
			return task.Interpretation{}, errors.New("unexpected evidence type")
		}
		if err := held.streams[predicate.Stdout].file.Close(); err != nil {
			return task.Interpretation{}, err
		}
		if err := raw.Read(predicate.Stdout, func(r io.Reader) error { _, e := io.Copy(io.Discard, r); return e }); err == nil {
			return task.Interpretation{}, errors.New("read unexpectedly succeeded")
		}
		return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "not an evidence fault"}, nil
	}
	_, td := interpreterTask(t, fn, "answer")
	out, cleanup, err := td.Collect(testInterpreterRef())
	must(t, cleanup)
	if err == nil || out != nil {
		t.Fatal("evidence read/close failure converted to rejection")
	}
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
}

func TestInterpreterSeesVerifiedHeldDescriptors(t *testing.T) {
	var td *TaskDir
	fn := func(_ predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		path := filepath.Join(td.Dir, "raw", "stdout")
		saved := filepath.Join(td.Dir, "raw", "held")
		if err := os.Rename(path, saved); err != nil {
			return task.Interpretation{}, err
		}
		if err := os.WriteFile(path, []byte("other inode"), 0o600); err != nil {
			return task.Interpretation{}, err
		}
		err := raw.Read(predicate.Stdout, func(r io.Reader) error { _, e := io.Copy(out, r); return e })
		err = errors.Join(err, os.Remove(path), os.Rename(saved, path))
		return task.Interpretation{Verdict: task.VerdictCommitted}, err
	}
	_, td = interpreterTask(t, fn, "sealed original")
	collectInterpreter(t, td)
	if string(readTestFile(t, filepath.Join(td.Dir, "result.txt"))) != "sealed original" {
		t.Fatal("parser reopened ambient raw path")
	}
}

func TestInvalidRawRefusesBeforeInterpreter(t *testing.T) {
	called := false
	fn := func(i predicate.Input, e predicate.Evidence, w io.Writer) (task.Interpretation, error) {
		called = true
		return streamInterpreter(i, e, w)
	}
	_, td := interpreterTask(t, fn, "sealed")
	writeTestFile(t, filepath.Join(td.Dir, "raw", "stdout"), []byte("changed"))
	out, cleanup, err := td.Collect(testInterpreterRef())
	must(t, cleanup)
	if out != nil || !errors.Is(err, task.ErrEvidenceFault) || called {
		t.Fatalf("invalid raw reached parser: %v %v %v", out, err, called)
	}
}
