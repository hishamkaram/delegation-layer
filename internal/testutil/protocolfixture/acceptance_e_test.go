package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAcceptance_GroupE(t *testing.T) {
	t.Parallel()
	t.Run("E01_EqualPayloadWithoutOutcome", testE01)
	t.Run("E02_ConflictingOrUnsafePayload", testE02)
	t.Run("E03_IdempotentWinnerWithoutReceipts", testE03)
	t.Run("E04_ConflictingCandidateReturnsWinner", testE04)
	t.Run("E05_InvalidWinnerNeverTerminal", testE05)
	t.Run("E06_BothPayloadNames", testE06)
	t.Run("E07_OverlappingSameCollectors", testE07)
	t.Run("E08_OverlappingDifferentCandidates", testE08)
	t.Run("E09_ConcurrentEqualAndConflictingRequests", testE09)
}

func testE01(t *testing.T) {
	for _, refused := range []bool{false, true} {
		name := "accepted"
		if refused {
			name = "refused"
		}
		t.Run(name, func(t *testing.T) {
			raw, answer, file, verdict := "  answer\n", "  answer\n", "result.txt", task.VerdictCommitted
			if refused {
				raw, answer, file, verdict = "", "empty_answer", "publish.reject", task.VerdictRejected
			}
			c := sealedCase(t, raw)
			writeTestFile(t, c.path(file), []byte(answer))
			before := snapshot(t, c.path(file))
			assertAbsent(t, c.path("outcome.json"))
			c.inspect(t, task.PublicationPending)
			c.collectExact(t, answer, verdict)
			c.collectExact(t, answer, verdict)
			assertSnapshot(t, c.path(file), before)
		})
	}
}

func testE02(t *testing.T) {
	for _, variant := range []string{"different-size", "same-size", "symlink", "directory"} {
		t.Run(variant, func(t *testing.T) {
			c := sealedCase(t, "good")
			path := c.path("result.txt")
			switch variant {
			case "different-size":
				writeTestFile(t, path, []byte("different"))
			case "same-size":
				writeTestFile(t, path, []byte("evil"))
			case "symlink":
				if err := os.Symlink(c.brief, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshot(t, path)
			external := snapshot(t, c.brief)
			c.run(t, 5, "collect")
			assertAbsent(t, c.path("outcome.json"))
			assertSnapshot(t, path, before)
			assertSnapshot(t, c.brief, external)
		})
	}
	t.Run("reserved-candidate-name", func(t *testing.T) {
		c := sealedCase(t, "good")
		c.run(t, 5, "candidate", "-file", "outcome.json", "-content", "poison")
		assertAbsent(t, c.path("outcome.json"))
		c.collectExact(t, "good", task.VerdictCommitted)
	})
}

func testE03(t *testing.T) {
	c := sealedCase(t, "  exact winner\n")
	c.collectExact(t, "  exact winner\n", task.VerdictCommitted)
	outcome, payload := snapshot(t, c.path("outcome.json")), snapshot(t, c.path("result.txt"))
	if err := os.Remove(c.path("provider.exit")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.path("publish.exit")); err == nil {
		if removeErr := os.Remove(c.path("publish.exit")); removeErr != nil {
			t.Fatal(removeErr)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	c.inspect(t, task.PublicationCommitted)
	c.collectExact(t, "  exact winner\n", task.VerdictCommitted)
	c.collectExact(t, "  exact winner\n", task.VerdictCommitted)
	assertSnapshot(t, c.path("outcome.json"), outcome)
	assertSnapshot(t, c.path("result.txt"), payload)
}

func TestAcceptance_HistoricalWorkspace(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"deleted", "renamed"} {
		for _, state := range []string{"sealed", "terminal"} {
			for _, verdict := range []string{task.VerdictCommitted, task.VerdictRejected} {
				t.Run(action+"/"+state+"/"+verdict, func(t *testing.T) {
					testHistoricalWorkspace(t, action, state, verdict)
				})
			}
		}
	}
}

func testHistoricalWorkspace(t *testing.T, action, state, verdict string) {
	t.Helper()
	raw, answer, publication := "  saved answer\n", "  saved answer\n", task.PublicationCommitted
	if verdict == task.VerdictRejected {
		raw, answer, publication = "", "empty_answer", task.PublicationRejected
	}
	c := sealedCase(t, raw)
	wantBefore := task.PublicationPending
	if state == "terminal" {
		c.collectExact(t, answer, verdict)
		wantBefore = publication
	}
	saved := capturePresent(t, c.path("task.json"), c.path("meta.json"), c.path("provider.exit"), c.path("raw/stdout"), c.path("raw/stderr"), c.path("outcome.json"), c.path("result.txt"), c.path("publish.reject"), c.sink)
	if action == "deleted" {
		if err := os.Remove(c.cwd); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Rename(c.cwd, filepath.Join(c.base, "moved-workspace")); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, c.cwd)
	c.inspect(t, wantBefore)
	c.collectExact(t, answer, verdict)
	c.collectExact(t, answer, verdict)
	c.inspect(t, publication)
	c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	c.run(t, 11, "claim", "-kind", "runner", "-fake-sink-event", c.sink)
	for path, before := range saved {
		assertSnapshot(t, path, before)
	}
	c.assertSink(t, "admission_sink", 1)
	c.assertSink(t, "runner_sink", 1)
}

func TestAcceptance_MissingWorkspaceRefusesFreshAuthority(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"prepare", "submission", "start"} {
		t.Run(boundary, func(t *testing.T) {
			c := newCase(t)
			if boundary != "prepare" {
				c.prepare(t)
			}
			if boundary == "start" {
				c.admit(t)
			}
			if err := os.Remove(c.cwd); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, c.sink)
			switch boundary {
			case "prepare":
				c.run(t, 2, "prepare", "-canonical-cwd", c.cwd, "-brief-file", c.brief)
			case "submission":
				c.run(t, 1, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
			case "start":
				c.run(t, 1, "seal", "-raw-stdout", "must not run", "-fake-sink-event", c.sink)
			}
			if boundary != "start" {
				assertAbsent(t, c.path("submit.json"))
			}
			assertAbsent(t, c.path("provider.start"))
			assertAbsent(t, c.path("provider.exit"))
			assertAbsent(t, c.path("raw/stdout"))
			assertAbsent(t, c.path("outcome.json"))
			assertSnapshot(t, c.sink, before)
		})
	}
}

func testE04(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*task.OutcomeRecord)
	}{
		{"task", func(r *task.OutcomeRecord) { r.TaskID = strings.Repeat("e", 32) }},
		{"spec", func(r *task.OutcomeRecord) { r.SpecSHA256 = task.ComputeSHA256([]byte("other-spec")) }},
		{"meta", func(r *task.OutcomeRecord) { r.MetaSHA256 = task.ComputeSHA256([]byte("other-meta")) }},
		{"evidence", func(r *task.OutcomeRecord) { r.EvidenceSHA256 = task.ComputeSHA256([]byte("other-evidence")) }},
		{"predicate", func(r *task.OutcomeRecord) { r.Predicate.Version = "unavailable" }},
		{"payload-descriptor", func(r *task.OutcomeRecord) { r.Payload.Length++ }},
	}
	c := sealedCase(t, "winner")
	c.collectExact(t, "winner", task.VerdictCommitted)
	original, payload := snapshot(t, c.path("outcome.json")), snapshot(t, c.path("result.txt"))
	result := c.run(t, 4, "candidate", "-file", "publish.reject", "-verdict", task.VerdictRejected, "-content", "candidate refusal")
	assertReturnedWinner(t, result, original.data)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var candidate task.OutcomeRecord
			if err := task.DecodeStrict(original.data, &candidate); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&candidate)
			path := filepath.Join(t.TempDir(), "candidate.json")
			saveJSON(t, path, candidate)
			result := c.run(t, 4, "candidate", "-candidate-record", path, "-content", "winner")
			assertReturnedWinner(t, result, original.data)
			assertSnapshot(t, c.path("outcome.json"), original)
			assertSnapshot(t, c.path("result.txt"), payload)
		})
	}
}

func assertReturnedWinner(t *testing.T, result fixtureResult, expected []byte) {
	t.Helper()
	var reply struct {
		Status, Error string
		Outcome       task.OutcomeRecord
	}
	if err := json.Unmarshal([]byte(result.stdout), &reply); err != nil {
		t.Fatal(err)
	}
	var winner task.OutcomeRecord
	if err := task.DecodeStrict(expected, &winner); err != nil {
		t.Fatal(err)
	}
	if reply.Status != "conflict" || reply.Error == "" || !task.CompareOutcomes(&reply.Outcome, &winner) {
		t.Fatalf("terminal winner plus separate conflict missing: %s", result.stdout)
	}
}

func testE05(t *testing.T) {
	for _, variant := range []string{"malformed", "duplicate", "unknown-version", "foreign-task", "foreign-meta", "missing-predicate", "empty-payload", "missing-payload", "changed-payload", "payload-symlink"} {
		t.Run(variant, func(t *testing.T) {
			c := sealedCase(t, "winner")
			c.collectExact(t, "winner", task.VerdictCommitted)
			corruptOutcome(t, c, variant)
			outcome := snapshot(t, c.path("outcome.json"))
			c.run(t, 5, "collect")
			c.run(t, 1, "inspect")
			assertSnapshot(t, c.path("outcome.json"), outcome)
		})
	}
}

func corruptOutcome(t *testing.T, c *fixtureCase, variant string) {
	t.Helper()
	path := c.path("outcome.json")
	switch variant {
	case "malformed":
		writeTestFile(t, path, []byte("NOT_JSON"))
	case "duplicate":
		data := readTestFile(t, path)
		data = append([]byte(`{"schema_version":1,`), data[1:]...)
		writeTestFile(t, path, data)
	case "unknown-version":
		mutateJSONField(t, path, "schema_version", json.RawMessage("999"))
	case "foreign-task":
		mutateJSONField(t, path, "task_id", json.RawMessage(`"`+strings.Repeat("e", 32)+`"`))
	case "foreign-meta":
		mutateJSONField(t, path, "meta_sha256", json.RawMessage(`"`+strings.Repeat("e", 64)+`"`))
	case "missing-predicate":
		mutateJSONField(t, path, "predicate", nil)
	case "empty-payload":
		var outcome task.OutcomeRecord
		if err := task.DecodeStrict(readTestFile(t, path), &outcome); err != nil {
			t.Fatal(err)
		}
		outcome.Payload.Length = 0
		outcome.Payload.SHA256 = task.ComputeSHA256(nil)
		saveJSON(t, path, outcome)
		writeTestFile(t, c.path("result.txt"), nil)
	case "missing-payload":
		if err := os.Remove(c.path("result.txt")); err != nil {
			t.Fatal(err)
		}
	case "payload-symlink":
		external := filepath.Join(c.base, "external-payload")
		writeTestFile(t, external, []byte("winner"))
		if err := os.Remove(c.path("result.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, c.path("result.txt")); err != nil {
			t.Fatal(err)
		}
	case "changed-payload":
		writeTestFile(t, c.path("result.txt"), []byte("changed"))
	}
}

func testE06(t *testing.T) {
	c := sealedCase(t, "answer")
	writeTestFile(t, c.path("result.txt"), []byte("answer"))
	writeTestFile(t, c.path("publish.reject"), []byte("stray refusal"))
	c.inspect(t, task.PublicationPending)
	c.collectExact(t, "answer", task.VerdictCommitted)
	outcome := snapshot(t, c.path("outcome.json"))
	extra := snapshot(t, c.path("publish.reject"))
	observation := c.inspect(t, task.PublicationCommitted)
	if observation.ExtraPayloadDiagnostic == "" {
		t.Fatal("extra payload diagnostic missing")
	}
	assertSnapshot(t, c.path("outcome.json"), outcome)
	assertSnapshot(t, c.path("publish.reject"), extra)
}

func testE07(t *testing.T) {
	c := sealedCase(t, "same winner")
	before := snapshot(t, c.sink)
	owner, _ := c.hold(t, "before-payload-link", "collect")
	c.run(t, 16, "collect")
	assertAbsent(t, c.path("outcome.json"))
	owner.releaseAndWait(t)
	outcome := snapshot(t, c.path("outcome.json"))
	c.collectExact(t, "same winner", task.VerdictCommitted)
	assertSnapshot(t, c.path("outcome.json"), outcome)
	assertSnapshot(t, c.sink, before)
}

func testE08(t *testing.T) {
	c := sealedCase(t, "sealed evidence")
	before := snapshot(t, c.sink)
	owner, _ := c.hold(t, "before-payload-link", "candidate", "-content", "first winner")
	c.run(t, 16, "candidate", "-content", "different loser")
	assertAbsent(t, c.path("outcome.json"))
	owner.releaseAndWait(t)
	outcome := snapshot(t, c.path("outcome.json"))
	payload := snapshot(t, c.path("result.txt"))
	result := c.run(t, 4, "candidate", "-content", "different loser")
	assertReturnedWinner(t, result, outcome.data)
	assertSnapshot(t, c.path("outcome.json"), outcome)
	assertSnapshot(t, c.path("result.txt"), payload)
	assertSnapshot(t, c.sink, before)
}

func testE09(t *testing.T) {
	c := newCase(t)
	owner, _ := c.hold(t, "after-task-barrier", "prepare", "-canonical-cwd", c.cwd, "-brief-file", c.brief)
	result := c.run(t, 1, "prepare", "-canonical-cwd", c.cwd, "-brief-file", c.brief)
	if !strings.Contains(result.stderr, "busy") {
		t.Fatalf("expected active admission lock: %s", result.stderr)
	}
	owner.releaseAndWait(t)
	c.prepare(t)
	request, meta := snapshot(t, c.path("task.json")), snapshot(t, c.path("meta.json"))
	result = c.run(t, 1, "prepare", "-canonical-cwd", c.cwd, "-brief-file", c.brief, "-budget", "45m")
	if !strings.Contains(result.stderr, "conflict") {
		t.Fatalf("expected immutable request conflict: %s", result.stderr)
	}
	submitter, _ := c.hold(t, "before-submit-consume", "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	submitter.releaseAndWait(t)
	c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	c.assertSink(t, "admission_sink", 1)
	assertSnapshot(t, c.path("task.json"), request)
	assertSnapshot(t, c.path("meta.json"), meta)
}
