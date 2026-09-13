package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type guardCrashCase struct {
	name, kind, point  string
	exit, retry, count int
	exists             bool
}

func TestAcceptance_GroupG(t *testing.T) {
	t.Parallel()
	t.Run("G01_PartialPreparedFragments", testG01)
	cases := []guardCrashCase{
		{"G02_BeforeSubmitLink", "admission", "before-submit-link", 3, 0, 1, false},
		{"G03_SubmitLinkedBeforeBarrier", "admission", "after-submit-link-before-barrier", 3, 10, 0, true},
		{"G04_SubmitBarrierFailure", "admission", "fail-submit-barrier", 13, 10, 0, true},
		{"G05_SubmitBarrierBeforeHandoff", "admission", "after-submit-barrier", 3, 10, 0, true},
		{"G06_PermitIssuedBeforeConsumption", "admission", "before-submit-consume", 3, 10, 0, true},
		{"G07_PermitConsumedBeforeSink", "admission", "after-submit-consume", 3, 10, 0, true},
		{"G09_BeforeStartLink", "runner", "before-start-link", 3, 0, 1, false},
		{"G10_StartLinkedBeforeBarrier", "runner", "after-start-link-before-barrier", 3, 11, 0, true},
		{"G11_StartBarrierFailure", "runner", "fail-start-barrier", 13, 11, 0, true},
		{"G12_StartBarrierBeforeHandoff", "runner", "after-start-barrier", 3, 11, 0, true},
		{"G13_StartSinkBeforeStartedReceipt", "runner", "after-start-sink", 3, 11, 1, true},
		{"G14_GuardCleanupCrash", "admission", "before-submit-cleanup", 3, 10, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runGuardCrash(t, tc)
		})
	}
	t.Run("G08_LostSubmissionReplyReconciliation", testG08)
	t.Run("G09_UnexplainedRawRefused", testG09Raw)
}

func runGuardCrash(t *testing.T, tc guardCrashCase) {
	c := newCase(t)
	c.prepare(t)
	record, event := "submit.json", "admission_sink"
	if tc.kind == "runner" {
		c.admit(t)
		record, event = "provider.start", "runner_sink"
	}
	rv := filepath.Join(t.TempDir(), "checkpoint")
	events := filepath.Join(t.TempDir(), "events")
	c.run(t, tc.exit, "claim", "-kind", tc.kind, "-checkpoint", tc.point, "-rendezvous", rv, "-events", events, "-fake-sink-event", c.sink)
	if tc.exit == 3 {
		assertCheckpoint(t, rv, tc.point)
	}
	guardPath := c.path(record)
	var before fileSnapshot
	if tc.exists {
		before = snapshot(t, guardPath)
	} else {
		assertAbsent(t, guardPath)
	}
	assertAbsent(t, c.path("provider.started.json"))
	assertAbsent(t, c.path("provider.exit"))
	firstCount := 0
	if tc.point == "after-start-sink" {
		firstCount = 1
	}
	c.assertSink(t, event, firstCount)
	c.run(t, tc.retry, "claim", "-kind", tc.kind, "-fake-sink-event", c.sink)
	c.assertSink(t, event, tc.count)
	if tc.exists {
		assertSnapshot(t, guardPath, before)
	}
	c.inspect(t, task.PublicationUnknown)
	if tc.point == "after-start-sink" {
		c.run(t, 14, "collect")
		assertAbsent(t, c.path("outcome.json"))
	}
	if tc.exit == 13 {
		assertEventOrder(t, events, "write:"+record, "file-barrier:"+record, "close:"+record, "link:"+record, "barrier:"+record)
	}
	if tc.name == "G14_GuardCleanupCrash" {
		c.run(t, 0, "scavenge")
		assertSnapshot(t, guardPath, before)
	}
}

func testG01(t *testing.T) {
	for _, fragment := range []string{"brief", "task", "meta"} {
		t.Run(fragment, func(t *testing.T) {
			c := newCase(t)
			point := "after-" + fragment + "-barrier"
			c.crash(t, point, "prepare", "-canonical-cwd", c.cwd, "-brief-file", c.brief)
			names := []string{"brief.md"}
			if fragment != "brief" {
				names = append(names, "task.json")
			}
			if fragment == "meta" {
				names = append(names, "meta.json")
			}
			before := make(map[string]fileSnapshot)
			for _, name := range names {
				before[name] = snapshot(t, c.path(name))
			}
			assertAbsent(t, c.path("submit.json"))
			assertAbsent(t, c.path("provider.start"))
			changed := filepath.Join(c.base, "different-brief.md")
			writeTestFile(t, changed, []byte("different request\n"))
			result := c.run(t, 1, "prepare", "-canonical-cwd", c.cwd, "-brief-file", changed)
			if !strings.Contains(result.stderr, "conflict") {
				t.Fatalf("wrong preparation error: %s", result.stderr)
			}
			for name, saved := range before {
				assertSnapshot(t, c.path(name), saved)
			}
			c.prepare(t)
			for name, saved := range before {
				assertSnapshot(t, c.path(name), saved)
			}
			c.assertSink(t, "admission_sink", 0)
			c.assertSink(t, "runner_sink", 0)
			c.inspect(t, task.PublicationUnknown)
		})
	}
}

func testG08(t *testing.T) {
	c := newCase(t)
	c.prepare(t)
	c.crash(t, "after-submit-sink", "claim", "-kind", "admission", "-fake-sink-event", c.sink)
	saved := snapshot(t, c.path("submit.json"))
	var submit task.SubmitRecord
	if err := task.DecodeStrict(saved.data, &submit); err != nil {
		t.Fatal(err)
	}
	one := `[{"root_id":"` + submit.RootID + `","task_id":"` + submit.TaskID + `","spec_sha256":"` + submit.SpecSHA256 + `","meta_sha256":"` + submit.MetaSHA256 + `"}]`
	for _, tc := range []struct {
		name, data, admission string
		code                  int
	}{{"one", one, "admitted", 0}, {"zero", "[]", "unknown", 3}, {"multiple", one[:len(one)-1] + "," + one[1:], "unknown", 3}, {"unreadable", "not JSON", "unknown", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "observation.json")
			writeTestFile(t, path, []byte(tc.data))
			result := c.run(t, tc.code, "reconcile", "-observations", path)
			if !strings.Contains(result.stdout, `"admission":"`+tc.admission+`"`) {
				t.Fatalf("wrong reconciliation: %s", result.stdout)
			}
			c.run(t, 10, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
			c.assertSink(t, "admission_sink", 1)
			assertSnapshot(t, c.path("submit.json"), saved)
		})
	}
}

func testG09Raw(t *testing.T) {
	for _, kind := range []string{"bytes", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			c := preparedCase(t)
			if err := os.MkdirAll(c.path("raw"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := c.path("raw/stdout")
			switch kind {
			case "bytes":
				writeTestFile(t, path, []byte("unexplained raw"))
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
			c.run(t, 1, "claim", "-kind", "runner", "-fake-sink-event", c.sink)
			assertAbsent(t, c.path("provider.start"))
			assertSnapshot(t, path, before)
			c.assertSink(t, "runner_sink", 0)
		})
	}
}
