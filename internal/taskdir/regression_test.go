package taskdir

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := InitStore(filepath.Join(t.TempDir(), "state"))
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	return s
}

func preparedInput(t *testing.T, s *Store) (*task.TaskRecord, *task.MetaRecord, []byte) {
	t.Helper()
	id, err := task.NewTaskID()
	must(t, err)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	brief := []byte("hello brief")
	config := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	req := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)), RequestedConfig: config}
	meta := &task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: id, Predicate: task.FixturePredicateRef(), RequestedConfig: config, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("effective"))}, Containment: "fixture-only", Approval: "never", ProviderExecutable: "/fake/provider", ProviderVersion: "fixture-v1", PublisherVersion: "test", PublisherBuild: "test-build", SupervisorConfig: task.SupervisorRef{ConfigPath: "/fake/pueue.yml", ConfigDigest: task.ComputeSHA256([]byte("pueue")), Endpoint: "/fake/socket", ObservedVersion: "fixture-v1"}, CreatedAt: timestamp()}
	return req, meta, brief
}

func preparedTask(t *testing.T, s *Store) *TaskDir {
	t.Helper()
	req, meta, brief := preparedInput(t, s)
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	return td
}

func consumeStart(t *testing.T, td *TaskDir) *StartPermit {
	t.Helper()
	p, err := td.PrepareStart(0)
	must(t, err)
	must(t, p.Consume())
	return p
}

func sealedTask(t *testing.T, s *Store, answer string) *TaskDir {
	t.Helper()
	td := preparedTask(t, s)
	p := consumeStart(t, td)
	must(t, td.RecordStarted(1))
	must(t, td.WriteRawFiles(answer, "diagnostic\n"))
	_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, p.Release())
	return td
}

func collectOK(t *testing.T, td *TaskDir) *task.OutcomeRecord {
	t.Helper()
	out, cleanup, err := td.Collect(task.FixturePredicateRef())
	must(t, err)
	must(t, cleanup)
	if out == nil {
		t.Fatal("missing winner")
	}
	return out
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	return b
}

func writeTestFile(t *testing.T, path string, b []byte) {
	t.Helper()
	must(t, os.WriteFile(path, b, 0o600))
}

func testAbsent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", path, err)
	}
}

func TestRunnerOwnershipAndWriterLifetime(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	if !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("fresh seal %v", err)
	}
	p, err := td.PrepareStart(0)
	must(t, err)
	if err = td.WriteRawFiles("unauthorized", ""); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
	must(t, p.Consume())
	if err = s.Close(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("closing active store released maintenance: %v", err)
	}
	must(t, td.RecordStarted(1))
	other, err := s.OpenTask(td.TaskID)
	must(t, err)
	defer closeTaskQuietly(other)
	_, cleanupErr, err := other.Collect(task.FixturePredicateRef())
	must(t, cleanupErr)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("collection under runner: %v", err)
	}
	if err = other.WriteRawFiles("foreign", ""); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
	w, err := td.OpenRawWriter("stdout")
	must(t, err)
	_, err = w.Write([]byte("exact\n"))
	must(t, err)
	if err = p.Release(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("release active writer %v", err)
	}
	_, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("seal active writer %v", err)
	}
	_, err = s.Scavenge()
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("maintenance released by nested RecordStarted staging: %v", err)
	}
	must(t, w.Close())
	_, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	if err = td.WriteRawFiles("late", ""); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal(err)
	}
	must(t, p.Release())
	out := collectOK(t, other)
	if out.Payload.Length != 6 {
		t.Fatalf("payload changed %+v", out.Payload)
	}
}

func TestPermitCopiesAndRelease(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	_, meta, err := td.PreparedRecords()
	must(t, err)
	p, err := td.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	copied := *p
	must(t, p.Consume())
	if err = copied.Consume(); !errors.Is(err, task.ErrPermitAlreadyUsed) {
		t.Fatal(err)
	}
	must(t, p.Release())
	td2 := preparedTask(t, s)
	_, meta, err = td2.PreparedRecords()
	must(t, err)
	q, err := td2.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	must(t, q.Release())
	if err = q.Consume(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
	start, err := td.PrepareStart(0)
	must(t, err)
	startCopy := *start
	leaseCopy := *start.RunnerLease()
	must(t, leaseCopy.Release())
	if err = startCopy.Consume(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
	var zero StartPermit
	if err = zero.Consume(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal(err)
	}
}

func TestPreparedIdentityAndStartBudget(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	if _, err := td.PrepareStart(int64(2 * time.Minute)); !errors.Is(err, task.ErrRequestConflict) {
		t.Fatal(err)
	}
	testAbsent(t, filepath.Join(td.Dir, "provider.start"))
	testAbsent(t, filepath.Join(td.Dir, "raw"))
	req, meta, err := td.PreparedRecords()
	must(t, err)
	p, err := td.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	must(t, p.Release())
	original := readTestFile(t, filepath.Join(td.Dir, "meta.json"))
	must(t, os.Remove(filepath.Join(td.Dir, "meta.json")))
	meta.PublisherBuild = "replacement"
	_, err = s.CreateTask(td.TaskID, req, []byte("hello brief"), meta)
	if !errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("guarded repair %v", err)
	}
	testAbsent(t, filepath.Join(td.Dir, "meta.json"))
	writeTestFile(t, filepath.Join(td.Dir, "meta.json"), original)
	var changed task.MetaRecord
	must(t, task.DecodeStrict(original, &changed))
	changed.PublisherBuild = "changed"
	data, err := task.MarshalCanonical(changed)
	must(t, err)
	writeTestFile(t, filepath.Join(td.Dir, "meta.json"), data)
	_, err = td.PrepareStart(0)
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed guarded metadata %v", err)
	}
}

func TestUnexpectedRawAndCanonicalRetry(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	must(t, os.Mkdir(filepath.Join(td.Dir, "raw"), 0o700))
	writeTestFile(t, filepath.Join(td.Dir, "raw", "stdout"), []byte("preexisting"))
	_, err := td.PrepareStart(0)
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal(err)
	}
	testAbsent(t, filepath.Join(td.Dir, "provider.start"))
	if string(readTestFile(t, filepath.Join(td.Dir, "raw", "stdout"))) != "preexisting" {
		t.Fatal("raw truncated")
	}
	req, meta, err := td.PreparedRecords()
	must(t, err)
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(req.CanonicalCwd, alias))
	req.CanonicalCwd = alias
	meta.CreatedAt = timestamp()
	retry, err := s.CreateTask(td.TaskID, req, []byte("hello brief"), meta)
	must(t, err)
	must(t, retry.Close())
	if req.CanonicalCwd == alias {
		t.Fatal("alias not normalized")
	}
}

func TestRootedRecordRefusal(t *testing.T) {
	for _, variant := range []string{"symlink", "mode", "fifo", "parent-symlink"} {
		t.Run(variant, func(t *testing.T) {
			s := testStore(t)
			td := preparedTask(t, s)
			_, meta, err := td.PreparedRecords()
			must(t, err)
			path := filepath.Join(td.Dir, "meta.json")
			switch variant {
			case "symlink":
				external := filepath.Join(t.TempDir(), "meta")
				writeTestFile(t, external, readTestFile(t, path))
				must(t, os.Remove(path))
				must(t, os.Symlink(external, path))
			case "mode":
				must(t, os.Chmod(path, 0o666))
			case "fifo":
				must(t, os.Remove(path))
				must(t, makeFIFO(path))
			case "parent-symlink":
				moved := td.Dir + "-moved"
				must(t, os.Rename(td.Dir, moved))
				must(t, os.Symlink(moved, td.Dir))
			default:
				t.Fatal(variant)
			}
			if _, err = td.PrepareSubmission(meta.SupervisorConfig); err == nil {
				t.Fatal("unsafe record granted authority")
			}
			testAbsent(t, filepath.Join(td.Dir, "submit.json"))
		})
	}
}

func TestPublicationAndWinnerAuthority(t *testing.T) {
	s := testStore(t)
	td := sealedTask(t, s, "  exact answer\n\n")
	_, cleanupErr, err := td.PublishCandidateForTest(task.VerdictCommitted, "outcome.json", []byte("poison"), task.FixturePredicateRef())
	must(t, cleanupErr)
	if err == nil {
		t.Fatal("accepted unsafe candidate")
	}
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
	winner := collectOK(t, td)
	bytesBefore := readTestFile(t, filepath.Join(td.Dir, "outcome.json"))
	if string(readTestFile(t, filepath.Join(td.Dir, "result.txt"))) != "  exact answer\n\n" {
		t.Fatal("answer changed")
	}
	must(t, os.Remove(filepath.Join(td.Dir, "provider.exit")))
	again := collectOK(t, td)
	if !reflect.DeepEqual(winner, again) {
		t.Fatal("winner changed without seal")
	}
	conflict, cleanupErr, err := td.PublishCandidateForTest(task.VerdictRejected, "publish.reject", []byte("wrong"), task.FixturePredicateRef())
	must(t, cleanupErr)
	if !errors.Is(err, task.ErrOutcomeConflict) || conflict == nil {
		t.Fatalf("lost winner: %v", err)
	}
	if !bytes.Equal(bytesBefore, readTestFile(t, filepath.Join(td.Dir, "outcome.json"))) {
		t.Fatal("winner mutated")
	}
	winner.SpecSHA256 = task.ComputeSHA256([]byte("foreign"))
	data, err := task.MarshalCanonical(winner)
	must(t, err)
	writeTestFile(t, filepath.Join(td.Dir, "outcome.json"), data)
	insp, err := td.Inspect()
	if err == nil || insp.Outcome != nil || insp.Publication != task.PublicationUnknown {
		t.Fatalf("foreign outcome terminal %+v %v", insp, err)
	}
	out, cleanupErr, err := td.Collect(task.FixturePredicateRef())
	must(t, cleanupErr)
	if err == nil || out != nil {
		t.Fatalf("foreign winner returned %+v %v", out, err)
	}
}

func TestUnsafeExistingPayload(t *testing.T) {
	for _, content := range []string{"answer", "answer and much longer target"} {
		t.Run(content, func(t *testing.T) {
			s := testStore(t)
			td := sealedTask(t, s, "answer")
			target := filepath.Join(t.TempDir(), "answer")
			writeTestFile(t, target, []byte(content))
			must(t, os.Symlink(target, filepath.Join(td.Dir, "result.txt")))
			out, cleanupErr, err := td.Collect(task.FixturePredicateRef())
			must(t, cleanupErr)
			if err == nil || out != nil {
				t.Fatalf("unsafe payload accepted %v", err)
			}
			testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
		})
	}
}

func TestSessionEvidenceAndReleaseRetry(t *testing.T) {
	s := testStore(t)
	a := preparedTask(t, s)
	b := preparedTask(t, s)
	provider := "fixture:test"
	conversation := "conversation"
	must(t, a.ClaimSession(provider, conversation))
	if err := b.ClaimSession(provider, conversation); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatal(err)
	}
	releasePath := filepath.Join(a.sessionDirectory(provider, conversation), a.TaskID+".release.json")
	writeTestFile(t, releasePath, []byte("{}"))
	if err := b.ClaimSession(provider, conversation); err == nil {
		t.Fatal("malformed release freed owner")
	}
	must(t, os.Remove(releasePath))
	fakeDigest := task.ComputeSHA256([]byte("fake"))
	writeTestFile(t, filepath.Join(a.Dir, "provider.exit"), []byte(`{"manifest_sha256":"`+fakeDigest+`","invocation_state":"start_failed"}`))
	if err := a.ReleaseSession(provider, conversation, fakeDigest); err == nil {
		t.Fatal("malformed seal released owner")
	}
	must(t, os.Remove(filepath.Join(a.Dir, "provider.exit")))
	p := consumeStart(t, a)
	must(t, a.WriteRawFiles("answer", ""))
	_, err := a.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, p.Release())
	winner := collectOK(t, a)
	must(t, a.ReleaseSession(provider, conversation, winner.EvidenceSHA256))
	must(t, b.ClaimSession(provider, conversation))
	must(t, a.ReleaseSession(provider, conversation, winner.EvidenceSHA256))
	if err = a.ClaimSession(provider, conversation); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatal(err)
	}
}

func TestStreamLargePayload(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	p := consumeStart(t, td)
	input := bytes.Repeat([]byte("long raw payload\n"), 100000)
	must(t, td.WriteRawFrom(bytes.NewReader(input), nil))
	_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, p.Release())
	out := collectOK(t, td)
	r, err := td.OpenPayload(out)
	must(t, err)
	actual, err := io.ReadAll(r)
	must(t, err)
	must(t, r.Close())
	if !bytes.Equal(input, actual) {
		t.Fatal("stream payload changed")
	}
}

func TestOutcomeSurvivesMalformedOptionalReceipts(t *testing.T) {
	s := testStore(t)
	td := sealedTask(t, s, "answer")
	winner := collectOK(t, td)
	for _, name := range []string{"provider.exit", "provider.started.json"} {
		writeTestFile(t, filepath.Join(td.Dir, name), []byte("broken optional receipt"))
	}
	insp, err := td.Inspect()
	must(t, err)
	if insp.Outcome == nil || !task.CompareOutcomes(insp.Outcome, winner) {
		t.Fatal("optional receipt overrode winner")
	}
	again := collectOK(t, td)
	if !task.CompareOutcomes(again, winner) {
		t.Fatal("collector lost winner")
	}
}

func TestOwnedFinalization(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	out, cleanup, err := td.Finalize(task.FixturePredicateRef())
	must(t, cleanup)
	if out != nil || !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal("fresh handle finalized")
	}
	p := consumeStart(t, td)
	w, err := td.OpenRawWriter("stdout")
	must(t, err)
	_, err = w.Write([]byte("owned answer"))
	must(t, err)
	out, cleanup, err = td.Finalize(task.FixturePredicateRef())
	must(t, cleanup)
	if out != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("active writer finalized")
	}
	must(t, w.Close())
	_, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	out, cleanup, err = td.Finalize(task.FixturePredicateRef())
	must(t, cleanup)
	must(t, err)
	if out == nil || out.Payload.SHA256 != task.ComputeSHA256([]byte("owned answer")) {
		t.Fatal("owned finalize changed answer")
	}
	if _, err = s.Scavenge(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("finalization released maintenance: %v", err)
	}
	other, err := s.OpenTask(td.TaskID)
	must(t, err)
	defer closeTaskQuietly(other)
	foreign, cleanup, err := other.Finalize(task.FixturePredicateRef())
	must(t, cleanup)
	if foreign != nil || !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal("foreign handle finalized")
	}
	must(t, p.Release())
	out, cleanup, err = td.Finalize(task.FixturePredicateRef())
	must(t, cleanup)
	if out != nil || !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatal("released handle finalized")
	}
	collectOK(t, other)
}

func TestMissingRawIsEvidenceFaultNotAbsentSeal(t *testing.T) {
	s := testStore(t)
	td := sealedTask(t, s, "answer")
	must(t, os.Remove(filepath.Join(td.Dir, "raw", "stdout")))
	out, cleanup, err := td.Collect(task.FixturePredicateRef())
	must(t, cleanup)
	if out != nil || !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, task.ErrNoSeal) {
		t.Fatalf("lost sealed raw classified as %v", err)
	}
	if _, err = os.Stat(filepath.Join(td.Dir, "provider.exit")); err != nil {
		t.Fatal(err)
	}
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
}

func TestSealedSuccessfulTerminationReleasesWithoutOutcome(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	must(t, td.ClaimSession("fixture:test", "natural-success"))
	p := consumeStart(t, td)
	must(t, td.WriteRawFiles("answer", ""))
	seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, p.Release())
	must(t, td.ReleaseSession("fixture:test", "natural-success", seal.ManifestSHA256))
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
	successor := preparedTask(t, s)
	must(t, successor.ClaimSession("fixture:test", "natural-success"))
}

func TestMissingStableLockIsNeverRecreated(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	req, meta, err := td.PreparedRecords()
	must(t, err)
	path := filepath.Join(td.Dir, ".run.lock")
	must(t, os.Remove(path))
	opened, err := s.OpenTask(td.TaskID)
	if err == nil || opened != nil {
		t.Fatal("OpenTask recreated missing run lock")
	}
	testAbsent(t, path)
	opened, err = s.CreateTask(td.TaskID, req, []byte("hello brief"), meta)
	if err == nil || opened != nil {
		t.Fatal("CreateTask recreated guarded stable lock")
	}
	testAbsent(t, path)
}
