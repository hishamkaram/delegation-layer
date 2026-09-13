package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type publicationCrashCase struct {
	name, point      string
	exit             int
	payload, outcome bool
}

func TestAcceptance_GroupR(t *testing.T) {
	t.Parallel()
	t.Run("R01_OwnedRawWriterExitsBeforeSeal", func(t *testing.T) { testUnsealedExit(t, "raw-writer-open") })
	t.Run("R02_DurableRawWithoutSeal", func(t *testing.T) { testUnsealedExit(t, "before-seal-link") })
	t.Run("R03_ExactSealedRecovery", testR03)
	t.Run("R04_InvalidSealedEvidence", testR04)
	t.Run("R05_ExactPredicateAndIdentity", testR05)
	cases := []publicationCrashCase{
		{"R06_BeforePayloadLink", "before-payload-link", 3, false, false},
		{"R07_PayloadLinkedBeforeBarrier", "after-payload-link-before-barrier", 3, true, false},
		{"R08_PayloadBarrierFailure", "fail-payload-barrier", 13, true, false},
		{"R09_PayloadDurableBeforeOutcome", "before-outcome-link", 3, true, false},
		{"R10_OutcomeLinkedBeforeBarrier", "after-outcome-link-before-barrier", 3, true, true},
		{"R11_OutcomeBarrierFailure", "fail-outcome-barrier", 13, true, true},
		{"R12_OutcomeDurableBeforeCleanup", "before-outcome-cleanup", 3, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("accepted", func(t *testing.T) { testPublicationCrash(t, tc, false) })
			t.Run("refused", func(t *testing.T) { testPublicationCrash(t, tc, true) })
		})
	}
	t.Run("R13_SealAndPayloadFailurePropagation", testR13)
	t.Run("R14_DefiniteStartFailure", testR14)
}

func testPublicationCrash(t *testing.T, tc publicationCrashCase, refused bool) {
	raw, answer, basename, verdict := "  exact answer\n", "  exact answer\n", "result.txt", task.VerdictCommitted
	if refused {
		raw, answer, basename, verdict = "", "empty_answer", "publish.reject", task.VerdictRejected
	}
	c := sealedCase(t, raw)
	sealBefore := snapshot(t, c.path("provider.exit"))
	rv, events := filepath.Join(t.TempDir(), "checkpoint"), filepath.Join(t.TempDir(), "events")
	c.run(t, tc.exit, "collect", "-checkpoint", tc.point, "-rendezvous", rv, "-events", events)
	if tc.exit == 3 {
		assertCheckpoint(t, rv, tc.point)
	}
	saved := capturePublicationCrash(t, c, tc, basename, answer, refused)
	if tc.outcome {
		c.run(t, 13, "collect", "-checkpoint", "fail-outcome-barrier")
		assertSnapshot(t, c.path("outcome.json"), saved["outcome.json"])
	}
	c.collectExact(t, answer, verdict)
	outcomeSaved := snapshot(t, c.path("outcome.json"))
	c.collectExact(t, answer, verdict)
	assertSnapshot(t, c.path("outcome.json"), outcomeSaved)
	assertSnapshot(t, c.path("provider.exit"), sealBefore)
	for name, before := range saved {
		assertSnapshot(t, c.path(name), before)
	}
	if tc.exit == 13 {
		record := basename
		if tc.outcome {
			record = "outcome.json"
		}
		assertEventOrder(t, events, "write:"+record, "file-barrier:"+record, "close:"+record, "link:"+record, "barrier:"+record)
	}
}

func capturePublicationCrash(t *testing.T, c *fixtureCase, tc publicationCrashCase, basename, answer string, refused bool) map[string]fileSnapshot {
	t.Helper()
	saved := make(map[string]fileSnapshot)
	if tc.payload {
		saved[basename] = snapshot(t, c.path(basename))
		if string(saved[basename].data) != answer {
			t.Fatal("wrong pending payload")
		}
	} else {
		assertAbsent(t, c.path(basename))
	}
	expected := task.PublicationPending
	if tc.outcome {
		saved["outcome.json"] = snapshot(t, c.path("outcome.json"))
		expected = task.PublicationCommitted
		if refused {
			expected = task.PublicationRejected
		}
		assertAbsent(t, c.path("publish.exit"))
	} else {
		assertAbsent(t, c.path("outcome.json"))
	}
	c.inspect(t, expected)
	return saved
}

func testUnsealedExit(t *testing.T, point string) {
	c := preparedCase(t)
	events := filepath.Join(t.TempDir(), "events")
	c.crash(t, point, "seal", "-raw-stdout", "success-looking unsealed bytes\n", "-fake-sink-event", c.sink, "-events", events)
	raw := snapshot(t, c.path("raw/stdout"))
	if string(raw.data) != "success-looking unsealed bytes\n" {
		t.Fatal("raw capture missing")
	}
	assertAbsent(t, c.path("provider.exit"))
	assertAbsent(t, c.path("outcome.json"))
	c.inspect(t, task.PublicationUnknown)
	c.run(t, 14, "collect")
	c.run(t, 11, "claim", "-kind", "runner", "-fake-sink-event", c.sink)
	c.assertSink(t, "runner_sink", 1)
	assertSnapshot(t, c.path("raw/stdout"), raw)
	if point == "before-seal-link" {
		assertEventOrder(t, events, "raw-barrier:stdout", "raw-barrier:stderr", "raw-directory-barrier:raw", "link:provider.exit")
	}
}

func testR03(t *testing.T) {
	for _, answer := range []string{"  exact answer\n", "  exact answer", "\n"} {
		t.Run(task.ComputeSHA256([]byte(answer))[:8], func(t *testing.T) {
			c := sealedCase(t, answer)
			before := snapshot(t, c.path("provider.exit"))
			c.collectExact(t, answer, task.VerdictCommitted)
			outcome := snapshot(t, c.path("outcome.json"))
			c.collectExact(t, answer, task.VerdictCommitted)
			assertSnapshot(t, c.path("outcome.json"), outcome)
			assertSnapshot(t, c.path("provider.exit"), before)
			c.assertSink(t, "admission_sink", 1)
			c.assertSink(t, "runner_sink", 1)
		})
	}
	t.Run("stream-larger-than-control-limit", func(t *testing.T) {
		c := preparedCase(t)
		answer := strings.Repeat(" raw line\n", task.MaxControlRecordSize/8)
		input := filepath.Join(c.base, "large-stdout")
		writeTestFile(t, input, []byte(answer))
		c.run(t, 0, "seal", "-raw-stdout-file", input, "-fake-sink-event", c.sink)
		c.collectExact(t, answer, task.VerdictCommitted)
	})
}

func readSeal(t *testing.T, c *fixtureCase) task.ProviderExitRecord {
	t.Helper()
	var seal task.ProviderExitRecord
	if err := task.DecodeStrict(readTestFile(t, c.path("provider.exit")), &seal); err != nil {
		t.Fatal(err)
	}
	return seal
}

func saveJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := task.MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, data)
}

func alterManifest(t *testing.T, c *fixtureCase, mutate func(*task.ProviderExitRecord)) {
	t.Helper()
	seal := readSeal(t, c)
	mutate(&seal)
	data, err := task.MarshalCanonical(seal.RawManifest)
	if err != nil {
		t.Fatal(err)
	}
	seal.ManifestSHA256 = task.ComputeSHA256(data)
	saveJSON(t, c.path("provider.exit"), seal)
}

func testR04(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *fixtureCase)
		code   int
	}{
		{"missing", func(t *testing.T, c *fixtureCase) {
			if err := os.Remove(c.path("raw/stdout")); err != nil {
				t.Fatal(err)
			}
		}, 15},
		{"changed-length", func(t *testing.T, c *fixtureCase) { writeTestFile(t, c.path("raw/stdout"), []byte("different length")) }, 15},
		{"changed-hash", func(t *testing.T, c *fixtureCase) { writeTestFile(t, c.path("raw/stdout"), []byte("evil")) }, 15},
		{"undeclared-stdout", func(t *testing.T, c *fixtureCase) {
			alterManifest(t, c, func(s *task.ProviderExitRecord) { s.RawManifest = s.RawManifest[:1] })
		}, 15},
		{"traversal", func(t *testing.T, c *fixtureCase) {
			alterManifest(t, c, func(s *task.ProviderExitRecord) { s.RawManifest[1].Path = "../outside" })
		}, 15},
		{"extra-entry", func(t *testing.T, c *fixtureCase) {
			alterManifest(t, c, func(s *task.ProviderExitRecord) {
				s.RawManifest = append(s.RawManifest, task.RawManifestEntry{Path: "raw/zz", SHA256: task.ComputeSHA256(nil)})
			})
		}, 15},
		{"raw-symlink", func(t *testing.T, c *fixtureCase) {
			path := c.path("raw/stdout")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(c.brief, path); err != nil {
				t.Fatal(err)
			}
		}, 15},
		{"raw-parent-symlink", func(t *testing.T, c *fixtureCase) {
			outside := filepath.Join(c.base, "preserved-raw")
			if err := os.Rename(c.path("raw"), outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, c.path("raw")); err != nil {
				t.Fatal(err)
			}
		}, 15},
		{"null-exit", func(t *testing.T, c *fixtureCase) {
			mutateJSONField(t, c.path("provider.exit"), "exit_code", json.RawMessage("null"))
		}, 15},
		{"absent-exit", func(t *testing.T, c *fixtureCase) { mutateJSONField(t, c.path("provider.exit"), "exit_code", nil) }, 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := sealedCase(t, "good")
			tc.mutate(t, c)
			rawBefore := capturePresent(t, c.path("raw/stdout"), c.path("raw/stderr"))
			seal := snapshot(t, c.path("provider.exit"))
			c.run(t, tc.code, "collect")
			assertAbsent(t, c.path("outcome.json"))
			assertAbsent(t, c.path("publish.reject"))
			assertSnapshot(t, c.path("provider.exit"), seal)
			c.run(t, 1, "inspect")
			for path, saved := range rawBefore {
				assertSnapshot(t, path, saved)
			}
			c.assertSink(t, "runner_sink", 1)
		})
	}
}

func mutateJSONField(t *testing.T, path, key string, value json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(readTestFile(t, path), &fields); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(fields, key)
	} else {
		fields[key] = value
	}
	saveJSON(t, path, fields)
}

func testR05(t *testing.T) {
	t.Run("unavailable-reference", func(t *testing.T) {
		c := sealedCase(t, "answer")
		c.run(t, 15, "collect", "-wrong-predicate")
		assertAbsent(t, c.path("outcome.json"))
		assertAbsent(t, c.path("publish.reject"))
		c.collectExact(t, "answer", task.VerdictCommitted)
	})
	for _, file := range []string{"task.json", "meta.json"} {
		t.Run(file, func(t *testing.T) {
			c := sealedCase(t, "answer")
			mutateJSONField(t, c.path(file), "task_id", json.RawMessage(`"`+strings.Repeat("f", 32)+`"`))
			before := snapshot(t, c.path(file))
			c.run(t, 1, "collect")
			assertAbsent(t, c.path("outcome.json"))
			assertAbsent(t, c.path("publish.reject"))
			assertSnapshot(t, c.path(file), before)
		})
	}
}

func testR13(t *testing.T) {
	for _, record := range []string{"seal", "payload"} {
		t.Run(record, func(t *testing.T) {
			for _, fault := range []string{"write", "short-write", "file-barrier", "close", "link"} {
				t.Run(fault, func(t *testing.T) {
					c := preparedCase(t)
					command := "seal"
					args := []string{"-raw-stdout", "answer"}
					if record == "payload" {
						c.seal(t, "answer")
						command = "collect"
						args = nil
					}
					point := "fail-" + record + "-" + fault
					events := filepath.Join(t.TempDir(), "events")
					c.run(t, 1, command, append(args, "-checkpoint", point, "-events", events)...)
					assertAbsent(t, c.path("outcome.json"))
					assertAbsent(t, c.path("result.txt"))
					assertAbsent(t, c.path("publish.reject"))
					if record == "seal" {
						assertAbsent(t, c.path("provider.exit"))
						c.run(t, 14, "collect")
					} else {
						c.collectExact(t, "answer", task.VerdictCommitted)
					}
				})
			}
		})
	}
}

func testR14(t *testing.T) {
	c := preparedCase(t)
	c.seal(t, "diagnostics are not an answer", "-inv-state", task.InvocationStartFailed, "-reason", "fixture refused start", "-exit-code", "0")
	assertAbsent(t, c.path("provider.started.json"))
	c.collectExact(t, "start_failed: fixture refused start", task.VerdictRejected)
	before := snapshot(t, c.path("provider.start"))
	c.run(t, 11, "claim", "-kind", "runner", "-fake-sink-event", c.sink)
	assertSnapshot(t, c.path("provider.start"), before)
	c.assertSink(t, "runner_sink", 0)
}
