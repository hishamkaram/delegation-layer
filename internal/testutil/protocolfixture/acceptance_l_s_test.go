package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAcceptance_GroupL_and_S(t *testing.T) {
	t.Parallel()
	t.Run("L01_AdmissionOwnerSelfExit", testL01)
	t.Run("L02_RunWriterExclusionAndExit", testL02)
	t.Run("L02_RunnerPublishesBeforeLeaseRelease", testRunnerPublishesBeforeLeaseRelease)
	t.Run("L03_ChildDoesNotInheritLock", testL03)
	t.Run("L04_ActiveStageBlocksScavenger", testL04)
	t.Run("L05_DifferentTasksMutateConcurrently", testL05)
	t.Run("L06_MaintenanceExclusiveBlocksMutation", testL06)
	t.Run("L07_ScavengeOnlyStages", testL07)
	t.Run("L08_CleanupFailuresPreserveAuthority", testL08)
	t.Run("L09_StableLockAndBoundedBusy", testL09)
	t.Run("S01_ConcurrentSessionClaims", testS01)
	t.Run("S02_ClaimOwnerCrashRemainsBusy", testS02)
	t.Run("S03_ReleaseRequiresCompleteEvidence", testS03)
	t.Run("S03_StopRequestsAreNotTermination", testSessionStopObservations)
	t.Run("S03_EstablishedTerminationReleases", testSessionEstablishedTermination)
}

func holdLock(t *testing.T, c *fixtureCase, level, mode string) *fixtureProcess {
	t.Helper()
	base := t.TempDir()
	rv, release := filepath.Join(base, "ready"), filepath.Join(base, "release")
	args := c.args("lock-hold", "-level", level, "-mode", mode, "-rendezvous", rv, "-wait-file", release, "-timeout-ms", "10000", "-no-unlock=true")
	p := startFixture(t, 0, release, args...)
	awaitCheckpoint(t, rv, "locked", p.cmd.Process.Pid)
	return p
}

func testL01(t *testing.T) {
	c := preparedCase(t)
	lock := snapshot(t, c.path(".admission.lock"))
	intent := snapshot(t, c.path("submit.json"))
	owner := holdLock(t, c, "admission", "ex")
	result := c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	if !strings.Contains(result.stderr, "busy") {
		t.Fatalf("owner exclusion not observed: %s", result.stderr)
	}
	owner.releaseAndWait(t)
	c.run(t, 0, "lock-hold", "-level", "admission", "-no-unlock=false", "-timeout-ms", "100")
	c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	assertSnapshot(t, c.path(".admission.lock"), lock)
	assertSnapshot(t, c.path("submit.json"), intent)
	c.assertSink(t, "admission_sink", 1)
}

func testL02(t *testing.T) {
	c := preparedCase(t)
	lock := snapshot(t, c.path(".run.lock"))
	owner, _ := c.hold(t, "raw-writer-open", "seal", "-raw-stdout", "writer-owned answer", "-fake-sink-event", c.sink)
	c.run(t, 16, "collect")
	c.run(t, 11, "claim", "-kind", "runner", "-fake-sink-event", c.sink)
	assertAbsent(t, c.path("provider.exit"))
	owner.releaseAndWait(t)
	c.collectExact(t, "writer-owned answer", task.VerdictCommitted)
	assertSnapshot(t, c.path(".run.lock"), lock)
	t.Run("self-exit-unsealed", func(t *testing.T) {
		c := preparedCase(t)
		lock := snapshot(t, c.path(".run.lock"))
		c.crash(t, "raw-writer-open", "seal", "-raw-stdout", "unsealed")
		c.run(t, 0, "lock-hold", "-level", "run", "-no-unlock=false", "-timeout-ms", "100")
		c.run(t, 11, "claim", "-kind", "runner")
		c.run(t, 14, "collect")
		assertSnapshot(t, c.path(".run.lock"), lock)
	})
}

func testRunnerPublishesBeforeLeaseRelease(t *testing.T) {
	c := preparedCase(t)
	lock := snapshot(t, c.path(".run.lock"))
	owner, _ := c.hold(t, "before-payload-link", "seal", "-publish", "-raw-stdout", "runner finalized answer", "-fake-sink-event", c.sink)
	c.run(t, 16, "collect")
	c.run(t, 11, "claim", "-kind", "runner", "-fake-sink-event", c.sink)
	assertAbsent(t, c.path("outcome.json"))
	owner.releaseAndWait(t)
	c.collectExact(t, "runner finalized answer", task.VerdictCommitted)
	assertSnapshot(t, c.path(".run.lock"), lock)
	c.assertSink(t, "runner_sink", 1)
}

func testL03(t *testing.T) {
	c := preparedCase(t)
	base := t.TempDir()
	ready, release, exited := filepath.Join(base, "child-ready"), filepath.Join(base, "child-release"), filepath.Join(base, "child-exited")
	output, err := os.OpenFile(filepath.Join(base, "parent.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fixtureBinPath, c.args("lock-hold", "-level", "admission", "-exec-child", "-child-ready-file", ready, "-child-wait-file", release, "-child-exit-file", exited, "-no-unlock=true")...)
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()
	closeErr := output.Close()
	if err := errors.Join(runErr, closeErr); err != nil {
		t.Fatal(err)
	}
	// A exited without defers. Child still waits for our release; B's 100ms bound
	// is much shorter than the child's 10s timeout, so inherited ownership fails.
	childPID := assertCheckpoint(t, ready, "waiting")
	assertAbsent(t, exited)
	t.Cleanup(func() {
		writeTestFile(t, release, []byte("release\n"))
		awaitCheckpoint(t, exited, "wait-exited-0", childPID)
	})
	c.run(t, 0, "lock-hold", "-level", "admission", "-no-unlock=false", "-timeout-ms", "100")
	assertAbsent(t, exited)
	writeTestFile(t, release, []byte("release\n"))
	awaitCheckpoint(t, exited, "wait-exited-0", childPID)
}

func testL04(t *testing.T) {
	c := sealedCase(t, "answer")
	owner, _ := c.hold(t, "before-payload-link", "collect")
	stages := recognizedStages(t, c.root)
	if len(stages) == 0 {
		t.Fatal("active staging file missing")
	}
	before := snapshot(t, stages[0])
	result := c.run(t, 1, "scavenge")
	if !strings.Contains(result.stderr, "busy") {
		t.Fatalf("expected maintenance busy: %s", result.stderr)
	}
	assertSnapshot(t, stages[0], before)
	owner.releaseAndWait(t)
	c.run(t, 0, "scavenge")
	c.collectExact(t, "answer", task.VerdictCommitted)
}

func siblingCase(t *testing.T, c *fixtureCase) *fixtureCase {
	t.Helper()
	copyCase := *c
	id, err := task.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	copyCase.id = id
	copyCase.sink = filepath.Join(c.base, "sink-"+id)
	writeTestFile(t, copyCase.sink, nil)
	return &copyCase
}

func testL05(t *testing.T) {
	a := sealedCase(t, "A answer")
	b := siblingCase(t, a)
	b.prepare(t)
	b.admit(t)
	b.seal(t, "B answer")
	ownerA, _ := a.hold(t, "before-payload-link", "collect")
	ownerB, _ := b.hold(t, "before-payload-link", "collect")
	assertAbsent(t, a.path("outcome.json"))
	assertAbsent(t, b.path("outcome.json"))
	ownerA.releaseAndWait(t)
	ownerB.releaseAndWait(t)
	a.collectExact(t, "A answer", task.VerdictCommitted)
	b.collectExact(t, "B answer", task.VerdictCommitted)
}

func testL06(t *testing.T) {
	c := newCase(t)
	c.prepare(t)
	owner := holdLock(t, c, "maintenance", "ex")
	result := c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	if !strings.Contains(result.stderr, "busy") {
		t.Fatalf("expected maintenance exclusion: %s", result.stderr)
	}
	assertAbsent(t, c.path("submit.json"))
	c.assertSink(t, "admission_sink", 0)
	owner.releaseAndWait(t)
	c.admit(t)
	c.assertSink(t, "admission_sink", 1)
}

func recognizedStages(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && isStageNameForTest(entry.Name()) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func isStageNameForTest(name string) bool {
	return len(name) == 42 && strings.HasPrefix(name, "stage.") && strings.HasSuffix(name, ".tmp") && task.ValidateTaskID(name[6:38]) == nil
}

func testL07(t *testing.T) {
	c := preparedCase(t)
	c.crash(t, "before-start-link", "claim", "-kind", "runner")
	stages := recognizedStages(t, c.root)
	if len(stages) == 0 {
		t.Fatal("exited writer left no recognized stage")
	}
	notes := c.path("custom.notes")
	writeTestFile(t, notes, []byte("unrelated notes"))
	invalidStage := "stage." + strings.Repeat("z", 32) + ".tmp"
	writeTestFile(t, c.path(invalidStage), []byte("unrecognized staging-looking note"))
	names := []string{"brief.md", "task.json", "meta.json", "submit.json", ".admission.lock", ".run.lock", "raw/stdout", "raw/stderr", "custom.notes", invalidStage}
	before := make(map[string]fileSnapshot)
	for _, name := range names {
		before[name] = snapshot(t, c.path(name))
	}
	c.run(t, 0, "scavenge")
	c.run(t, 0, "scavenge")
	for _, stage := range stages {
		assertAbsent(t, stage)
	}
	for name, saved := range before {
		assertSnapshot(t, c.path(name), saved)
	}
}

func testL08(t *testing.T) {
	for _, fault := range []string{"cleanup", "cleanup-barrier"} {
		t.Run(fault, func(t *testing.T) {
			c := newCase(t)
			c.prepare(t)
			events := filepath.Join(t.TempDir(), "events")
			c.run(t, 1, "claim", "-kind", "admission", "-checkpoint", "fail-submit-"+fault, "-events", events, "-fake-sink-event", c.sink)
			before := snapshot(t, c.path("submit.json"))
			c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
			c.assertSink(t, "admission_sink", 0)
			c.run(t, 0, "scavenge")
			assertSnapshot(t, c.path("submit.json"), before)
			assertEventOrder(t, events, "barrier-complete:submit.json", "cleanup:submit.json")
		})
	}
}

func testL09(t *testing.T) {
	// Syscall error variants are TestLockFailuresAndRetry in internal/taskdir.
	// This subprocess control proves bounded contention and stable inode identity.
	c := newCase(t)
	c.prepare(t)
	before := snapshot(t, c.path(".admission.lock"))
	owner := holdLock(t, c, "admission", "ex")
	c.run(t, 1, "lock-hold", "-level", "admission", "-no-unlock=false", "-timeout-ms", "50")
	owner.releaseAndWait(t)
	c.run(t, 0, "lock-hold", "-level", "admission", "-no-unlock=false", "-timeout-ms", "50")
	assertSnapshot(t, c.path(".admission.lock"), before)
}

func testS01(t *testing.T) {
	a := newCase(t)
	a.prepare(t)
	b := siblingCase(t, a)
	b.prepare(t)
	prepared := capturePresent(t, a.path("task.json"), a.path("meta.json"), b.path("task.json"), b.path("meta.json"), a.sink, b.sink)
	owner, _ := a.hold(t, "before-session-claim-record-link", "session", "-action", "claim", "-conv-id", "concurrent")
	// A has examined ownership and staged its claim while holding the session
	// lock. B must be excluded before either durable claim exists.
	assertNoSessionClaims(t, a.root)
	result := b.run(t, 20, "session", "-action", "claim", "-conv-id", "concurrent")
	if !strings.Contains(result.stderr, "busy") {
		t.Fatalf("critical-section exclusion not observed: %s", result.stderr)
	}
	assertNoSessionClaims(t, a.root)
	owner.releaseAndWait(t)
	records := sessionRecords(t, a, "concurrent")
	assertSingleSessionOwner(t, records, a.id, "concurrent")
	a.run(t, 0, "session", "-action", "claim", "-conv-id", "concurrent")
	b.run(t, 20, "session", "-action", "claim", "-conv-id", "concurrent")
	for path, before := range records {
		assertSnapshot(t, path, before)
	}
	assertSingleSessionOwner(t, sessionRecords(t, a, "concurrent"), a.id, "concurrent")
	for path, before := range prepared {
		assertSnapshot(t, path, before)
	}
	for _, c := range []*fixtureCase{a, b} {
		assertAbsent(t, c.path("submit.json"))
		assertAbsent(t, c.path("provider.start"))
	}
}

func assertNoSessionClaims(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(root, "sessions"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".claim.json") {
			t.Errorf("unexpected committed claim while first owner is paused: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertSingleSessionOwner(t *testing.T, records map[string]fileSnapshot, taskID, conversation string) {
	t.Helper()
	if len(records) != 1 {
		t.Fatalf("expected exactly one immutable session claim, got %d", len(records))
	}
	for path, saved := range records {
		var claim task.SessionClaimRecord
		if err := task.DecodeStrict(saved.data, &claim); err != nil {
			t.Fatal(err)
		}
		if err := task.ValidateSessionClaimRecord(&claim); err != nil {
			t.Fatal(err)
		}
		if claim.TaskID != taskID || claim.Provider != config.ProviderFixture || claim.ConversationID != conversation || filepath.Base(path) != taskID+".claim.json" {
			t.Fatalf("wrong immutable session owner: %s %+v", path, claim)
		}
	}
}

func testS02(t *testing.T) {
	a := newCase(t)
	a.prepare(t)
	b := siblingCase(t, a)
	b.prepare(t)
	a.crash(t, "after-session-claim", "session", "-action", "claim", "-conv-id", "stranded")
	assertAbsent(t, a.path("submit.json"))
	assertAbsent(t, a.path("provider.start"))
	before := sessionRecords(t, a, "stranded")
	b.run(t, 20, "session", "-action", "claim", "-conv-id", "stranded")
	for path, saved := range before {
		assertSnapshot(t, path, saved)
	}
}

func sessionRecords(t *testing.T, c *fixtureCase, conversation string) map[string]fileSnapshot {
	t.Helper()
	root := filepath.Join(c.root, "sessions")
	records := make(map[string]fileSnapshot)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		data := readTestFile(t, path)
		if strings.Contains(string(data), `"conversation_id":"`+conversation+`"`) {
			records[path] = snapshot(t, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("no durable conversation record")
	}
	return records
}

func testS03(t *testing.T) {
	a := newCase(t)
	a.prepare(t)
	a.run(t, 0, "session", "-action", "claim", "-conv-id", "release-proof")
	b := siblingCase(t, a)
	b.prepare(t, "-prior-task-id", a.id, "-prior-conversation", "release-proof")
	original := snapshot(t, a.path("task.json"))
	a.run(t, 1, "session", "-action", "release", "-conv-id", "release-proof", "-evidence-digest", "")
	for _, evidence := range []string{strings.Repeat("f", 64)} {
		a.run(t, 20, "session", "-action", "release", "-conv-id", "release-proof", "-evidence-digest", evidence)
		b.run(t, 20, "session", "-action", "claim", "-conv-id", "release-proof")
	}
	// A malformed two-field start_failed record is not termination evidence.
	writeTestFile(t, a.path("provider.exit"), []byte(`{"manifest_sha256":"`+strings.Repeat("f", 64)+`","invocation_state":"start_failed"}`))
	a.run(t, 20, "session", "-action", "release", "-conv-id", "release-proof", "-evidence-digest", strings.Repeat("f", 64))
	if err := os.Remove(a.path("provider.exit")); err != nil {
		t.Fatal(err)
	}
	a.admit(t)
	a.seal(t, "terminal answer")
	a.collectExact(t, "terminal answer", task.VerdictCommitted)
	var outcome task.OutcomeRecord
	if err := task.DecodeStrict(readTestFile(t, a.path("outcome.json")), &outcome); err != nil {
		t.Fatal(err)
	}
	a.run(t, 1, "session", "-action", "release", "-conv-id", "release-proof", "-evidence-digest", strings.Repeat("f", 64))
	for range 2 {
		a.run(t, 0, "session", "-action", "release", "-conv-id", "release-proof", "-evidence-digest", outcome.EvidenceSHA256)
	}
	records := sessionRecords(t, a, "release-proof")
	checkReleaseReceipts(t, records, a.id, outcome.EvidenceSHA256)
	b.run(t, 0, "session", "-action", "claim", "-conv-id", "release-proof")
	a.run(t, 0, "session", "-action", "release", "-conv-id", "release-proof", "-evidence-digest", outcome.EvidenceSHA256)
	assertSnapshot(t, a.path("task.json"), original)
	checkContinuation(t, b, a.id)
	for path, saved := range records {
		assertSnapshot(t, path, saved)
	}
}

func checkReleaseReceipts(t *testing.T, records map[string]fileSnapshot, taskID, evidence string) {
	t.Helper()
	found := false
	for _, saved := range records {
		var release task.SessionReleaseRecord
		if err := json.Unmarshal(saved.data, &release); err != nil {
			t.Fatal(err)
		}
		if release.PredecessorEvidenceSHA256 != "" {
			found = true
			if release.PredecessorEvidenceSHA256 != evidence || release.TaskID != taskID || release.Provider != config.ProviderFixture {
				t.Fatal("release receipt identity mismatch")
			}
		}
	}
	if !found {
		t.Fatal("missing immutable release receipt")
	}
}

func checkContinuation(t *testing.T, b *fixtureCase, predecessor string) {
	t.Helper()
	var continuation task.TaskRecord
	if err := task.DecodeStrict(readTestFile(t, b.path("task.json")), &continuation); err != nil {
		t.Fatal(err)
	}
	if continuation.TaskID == predecessor || continuation.Provider != config.ProviderFixture || continuation.Mode != config.ModeReadOnly || continuation.PriorSession == nil || continuation.PriorSession.PredecessorTaskID != predecessor || continuation.PriorSession.ConversationID != "release-proof" {
		t.Fatalf("wrong immutable continuation: %+v", continuation)
	}
}

func testSessionStopObservations(t *testing.T) {
	c := newCase(t)
	c.prepare(t)
	c.run(t, 0, "session", "-action", "claim", "-conv-id", "stop-observations")
	if err := os.Mkdir(c.path("stop"), 0o700); err != nil {
		t.Fatal(err)
	}
	var request task.TaskRecord
	if err := task.DecodeStrict(readTestFile(t, c.path("task.json")), &request); err != nil {
		t.Fatal(err)
	}
	var meta task.MetaRecord
	if err := task.DecodeStrict(readTestFile(t, c.path("meta.json")), &meta); err != nil {
		t.Fatal(err)
	}
	for name, record := range map[string]any{
		"request":  task.StopRequestRecord{SchemaVersion: 1, RootID: request.RootID, TaskID: c.id, SpecSHA256: meta.SpecSHA256, RequestID: "budget", Cause: "budget", Supervisor: meta.SupervisorConfig, RequestedAt: "2026-09-13T00:00:00Z"},
		"reply":    task.StopReplyRecord{SchemaVersion: 1, RootID: request.RootID, TaskID: c.id, SpecSHA256: meta.SpecSHA256, RequestID: "budget", Acknowledged: true, Message: "stop acknowledged", RepliedAt: "2026-09-13T00:00:00Z"},
		"observed": task.StopObservedRecord{SchemaVersion: 1, RootID: request.RootID, TaskID: c.id, SpecSHA256: meta.SpecSHA256, RequestID: "budget", Terminated: false, ObservedAt: "2026-09-13T00:00:00Z"},
	} {
		path := c.path("stop/budget." + name + ".json")
		saveJSON(t, path, record)
		before := snapshot(t, path)
		c.run(t, 20, "session", "-action", "release", "-conv-id", "stop-observations", "-evidence-digest", strings.Repeat("a", 64))
		assertSnapshot(t, path, before)
	}
	c.assertSink(t, "admission_sink", 0)
	c.assertSink(t, "runner_sink", 0)
}

func testSessionEstablishedTermination(t *testing.T) {
	a := newCase(t)
	a.prepare(t)
	a.run(t, 0, "session", "-action", "claim", "-conv-id", "terminated")
	a.admit(t)
	a.seal(t, "", "-inv-state", task.InvocationStartFailed, "-reason", "definite start failure")
	assertAbsent(t, a.path("outcome.json"))
	seal := readSeal(t, a)
	before := snapshot(t, a.path("provider.exit"))
	a.run(t, 0, "session", "-action", "release", "-conv-id", "terminated", "-evidence-digest", seal.ManifestSHA256)
	b := siblingCase(t, a)
	b.prepare(t, "-prior-task-id", a.id, "-prior-conversation", "terminated")
	b.run(t, 0, "session", "-action", "claim", "-conv-id", "terminated")
	assertSnapshot(t, a.path("provider.exit"), before)
	assertAbsent(t, a.path("outcome.json"))
	a.assertSink(t, "runner_sink", 0)
}
