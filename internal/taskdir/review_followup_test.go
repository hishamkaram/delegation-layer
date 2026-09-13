package taskdir

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecoverySurvivesUnavailableWorkspace(t *testing.T) {
	for _, mode := range []string{"deleted", "renamed"} {
		for _, published := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-sealed", true: "-terminal"}[published], func(t *testing.T) {
				s := testStore(t)
				td := preparedTask(t, s)
				must(t, td.ClaimSession("fixture:test", "historical-cwd"))
				req, _, err := td.PreparedRecords()
				must(t, err)
				p := consumeStart(t, td)
				must(t, td.WriteRawFiles("saved answer", ""))
				seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
				must(t, err)
				must(t, p.Release())
				if published {
					collectOK(t, td)
				}
				if mode == "deleted" {
					must(t, os.Remove(req.CanonicalCwd))
				} else {
					must(t, os.Rename(req.CanonicalCwd, req.CanonicalCwd+"-renamed"))
				}
				out := collectOK(t, td)
				if out.Payload.SHA256 != task.ComputeSHA256([]byte("saved answer")) {
					t.Fatal("saved result changed")
				}
				insp, err := td.Inspect()
				must(t, err)
				if insp.Publication != task.PublicationCommitted {
					t.Fatal("saved result not terminal")
				}
				must(t, td.ReleaseSession("fixture:test", "historical-cwd", seal.ManifestSHA256))
			})
		}
	}
}

func TestFreshAuthorityRefusesUnavailableWorkspace(t *testing.T) {
	for _, kind := range []string{"submit", "start"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			td := preparedTask(t, s)
			req, meta, err := td.PreparedRecords()
			must(t, err)
			must(t, os.Remove(req.CanonicalCwd))
			if kind == "submit" {
				p, e := td.PrepareSubmission(meta.SupervisorConfig)
				if p != nil || e == nil {
					t.Fatal("new admission with unavailable cwd")
				}
				testAbsent(t, filepath.Join(td.Dir, "submit.json"))
			} else {
				p, e := td.PrepareStart(0)
				if p != nil || e == nil {
					t.Fatal("new start with unavailable cwd")
				}
				testAbsent(t, filepath.Join(td.Dir, "provider.start"))
				testAbsent(t, filepath.Join(td.Dir, "raw"))
			}
		})
	}
}

func TestBootstrapAncestorRetryBarrier(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	target := filepath.Join(base, "new-ancestor", "child", "state")
	hook := &orderedFault{target: filepath.Base(base), fail: "parent"}
	if err = createDirectoriesWithAncestorBarrier(target, hook); err == nil {
		t.Fatal("failed initial ancestor barrier ignored")
	}
	if _, err = os.Stat(filepath.Join(base, "new-ancestor")); err != nil {
		t.Fatal("failed before ancestor became visible")
	}
	testAbsent(t, target)
	if err = createDirectoriesWithAncestorBarrier(target, hook); err == nil {
		t.Fatal("retry ignored still-failing ancestor barrier")
	}
	testAbsent(t, filepath.Join(base, "new-ancestor", "child"))
	hook.fail = ""
	hook.events = nil
	must(t, createDirectoriesWithAncestorBarrier(target, hook))
	repair := slices.Index(hook.events, "parent:"+filepath.Base(base))
	child := slices.Index(hook.events, "directory:child")
	if repair < 0 || child < 0 || repair >= child {
		t.Fatalf("retry omitted ancestor repair before new child: %v", hook.events)
	}
}

type failedCaptureReader struct{ sent bool }

func (r *failedCaptureReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	return copy(p, []byte("truncated answer")), errors.New("source capture failed")
}

func TestSourceCaptureFailurePreventsSeal(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	p := consumeStart(t, td)
	defer func() { must(t, p.Release()) }()
	if err := td.WriteRawFrom(&failedCaptureReader{}, nil); err == nil {
		t.Fatal("source read failure missing")
	}
	seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	if seal != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("source failure allowed sealing: %+v %v", seal, err)
	}
	testAbsent(t, filepath.Join(td.Dir, "provider.exit"))
}

func TestSealRetryRepairsUncertainBarrier(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	p := consumeStart(t, td)
	defer func() { must(t, p.Release()) }()
	must(t, td.WriteRawFiles("answer", ""))
	hook := &orderedFault{target: "provider.exit", fail: "post-link"}
	s.SetFaultInjector(hook)
	seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	if seal != nil || !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatal("missing initial uncertain seal")
	}
	original := readTestFile(t, filepath.Join(td.Dir, "provider.exit"))
	hook.events = nil
	seal, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	if seal != nil || !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatal("same-live-lease retry acknowledged while barrier still fails")
	}
	hook.fail = ""
	hook.events = nil
	seal, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	if seal == nil {
		t.Fatal("missing recovered seal")
	}
	if !containsEvent(hook.events, "post-link:provider.exit") || containsEvent(hook.events, "link:provider.exit") {
		t.Fatalf("retry lacked post-existing seal barrier: %v", hook.events)
	}
	if string(original) != string(readTestFile(t, filepath.Join(td.Dir, "provider.exit"))) {
		t.Fatal("seal retry changed bytes")
	}
}

func TestSessionProofRepairsUncertainBarrier(t *testing.T) {
	for _, record := range []string{"provider.exit", "outcome.json"} {
		t.Run(record, func(t *testing.T) { testUncertainSessionProof(t, record) })
	}
}

func testUncertainSessionProof(t *testing.T, record string) {
	s := testStore(t)
	td := preparedTask(t, s)
	must(t, td.ClaimSession("fixture:test", "uncertain-proof"))
	hook := createUncertainSessionProof(t, td, record)
	_, _, meta, spec, metaHash, err := td.loadAndValidatePreparedSet()
	must(t, err)
	seal, err := td.readSeal(meta.Predicate, spec, metaHash)
	must(t, err)
	before := readTestFile(t, filepath.Join(td.Dir, record))
	infoBefore, err := os.Stat(filepath.Join(td.Dir, record))
	must(t, err)
	if err = td.ReleaseSession("fixture:test", "uncertain-proof", seal.ManifestSHA256); !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("uncertain proof acknowledged: %v", err)
	}
	testAbsent(t, filepath.Join(td.sessionDirectory("fixture:test", "uncertain-proof"), td.TaskID+".release.json"))
	hook.fail = ""
	hook.events = nil
	must(t, td.ReleaseSession("fixture:test", "uncertain-proof", seal.ManifestSHA256))
	if !containsEvent(hook.events, "post-link:"+record) || containsEvent(hook.events, "link:"+record) {
		t.Fatalf("proof lacked existing-record barrier: %v", hook.events)
	}
	infoAfter, err := os.Stat(filepath.Join(td.Dir, record))
	must(t, err)
	if !os.SameFile(infoBefore, infoAfter) || string(before) != string(readTestFile(t, filepath.Join(td.Dir, record))) {
		t.Fatal("proof rewritten")
	}
}

func createUncertainSessionProof(t *testing.T, td *TaskDir, record string) *orderedFault {
	t.Helper()
	p := consumeStart(t, td)
	must(t, td.WriteRawFiles("answer", ""))
	hook := &orderedFault{target: record, fail: "post-link"}
	if record == "provider.exit" {
		td.store.SetFaultInjector(hook)
	}
	seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	if record == "provider.exit" {
		if seal != nil || !errors.Is(err, task.ErrUncertainDurability) {
			t.Fatal(err)
		}
	} else {
		must(t, err)
	}
	must(t, p.Release())
	if record == "outcome.json" {
		td.store.SetFaultInjector(hook)
		out, cleanup, collectErr := td.Collect(task.FixturePredicateRef())
		must(t, cleanup)
		if out != nil || !errors.Is(collectErr, task.ErrUncertainDurability) {
			t.Fatal(collectErr)
		}
	}
	return hook
}

type stageCreationCollision struct {
	BaseFaultInjector
	path string
	info os.FileInfo
}

func (h *stageCreationCollision) OnBeforeStageCreate(stage, _ string) error {
	h.path = stage
	f, err := os.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte("preexisting stage sentinel"))
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	h.info, err = os.Stat(stage)
	return err
}

func TestStageCreationCollisionPreservesExistingFile(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	hook := &stageCreationCollision{}
	committed, cleanup, err := s.stageAndCommit(td.Dir, "stage-collision.json", []byte("replacement"), hook)
	must(t, cleanup)
	if committed || !errors.Is(err, os.ErrExist) {
		t.Fatalf("stage O_EXCL collision ignored: %v", err)
	}
	info, err := os.Stat(hook.path)
	must(t, err)
	if !os.SameFile(info, hook.info) || string(readTestFile(t, hook.path)) != "preexisting stage sentinel" {
		t.Fatal("exclusive stage creation mutated existing file")
	}
	testAbsent(t, filepath.Join(td.Dir, "stage-collision.json"))
	for _, name := range []string{"submit.json", "provider.start", "outcome.json"} {
		testAbsent(t, filepath.Join(td.Dir, name))
	}
	committed, cleanup, err = s.stageAndCommit(td.Dir, "stage-collision.json", []byte("fresh stage"), nil)
	must(t, err)
	must(t, cleanup)
	if !committed || string(readTestFile(t, filepath.Join(td.Dir, "stage-collision.json"))) != "fresh stage" {
		t.Fatal("fresh stage control did not commit")
	}
	info, err = os.Stat(hook.path)
	must(t, err)
	if !os.SameFile(info, hook.info) || string(readTestFile(t, hook.path)) != "preexisting stage sentinel" {
		t.Fatal("control mutated collision")
	}
}

func TestTaskNormalizationIsTransactional(t *testing.T) {
	s := testStore(t)
	req, _, _ := preparedInput(t, s)
	req.Mode = ""
	req.RequestedConfig = task.TaskConfig{}
	req.BudgetNanos = 0
	old := *req
	must(t, os.Remove(req.CanonicalCwd))
	if err := NormalizeTaskRecord(s.Root, req); err == nil {
		t.Fatal("missing workspace normalized")
	}
	if !reflect.DeepEqual(old, *req) {
		t.Fatal("failed normalization changed caller")
	}
	must(t, os.Mkdir(req.CanonicalCwd, 0o700))
	must(t, NormalizeTaskRecord(s.Root, req))
	if req.Mode != "read-only" || req.RequestedConfig.Permission != req.Mode || req.BudgetNanos <= 0 {
		t.Fatal("normalization lost defaults")
	}
	must(t, task.ValidateTaskRecord(req))
}

func TestContinuationClaimMatchesSavedConversation(t *testing.T) {
	s := testStore(t)
	req, meta, brief := preparedInput(t, s)
	predecessor, err := task.NewTaskID()
	must(t, err)
	req.PriorSession = &task.PriorSession{Provider: req.Provider, ConversationID: "saved-conversation", PredecessorTaskID: predecessor}
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	defer closeTaskQuietly(td)
	if err = td.ClaimSession(req.Provider, "wrong-conversation"); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("wrong conversation claim accepted: %v", err)
	}
	testAbsent(t, td.sessionDirectory(req.Provider, "wrong-conversation"))
	testAbsent(t, filepath.Join(td.Dir, "submit.json"))
	testAbsent(t, filepath.Join(td.Dir, "provider.start"))
	must(t, td.ClaimSession(req.Provider, "saved-conversation"))
	claim, err := td.readClaim(td.sessionDirectory(req.Provider, "saved-conversation"), td.TaskID, req.Provider, "saved-conversation")
	must(t, err)
	if claim.ConversationID != req.PriorSession.ConversationID {
		t.Fatal("claim lost saved binding")
	}
}
