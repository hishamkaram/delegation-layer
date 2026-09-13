package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAcceptance_StartedReceiptCheckpoints(t *testing.T) {
	t.Parallel()
	cases := []struct {
		point   string
		exit    int
		present bool
	}{
		{"before-started-link", 3, false},
		{"after-started-link-before-barrier", 3, true},
		{"after-started-barrier", 3, true},
		{"fail-started-write", 1, false},
		{"fail-started-barrier", 13, true},
	}
	for _, tc := range cases {
		t.Run(tc.point, func(t *testing.T) {
			c := preparedCase(t)
			rv, events := filepath.Join(t.TempDir(), "checkpoint"), filepath.Join(t.TempDir(), "events")
			c.run(t, tc.exit, "seal", "-raw-stdout", "unreached answer", "-checkpoint", tc.point, "-rendezvous", rv, "-events", events, "-fake-sink-event", c.sink)
			assertCheckpoint(t, rv, tc.point)
			if tc.present {
				validateStartedReceipt(t, c)
			} else {
				assertAbsent(t, c.path("provider.started.json"))
			}
			assertStartedTrace(t, events, tc.point)
			assertStartedInterruption(t, c)
		})
	}
	t.Run("normal-started-control", func(t *testing.T) {
		c := sealedCase(t, "normal started answer")
		validateStartedReceipt(t, c)
		c.collectExact(t, "normal started answer", task.VerdictCommitted)
		c.assertSink(t, "runner_sink", 1)
	})
}

func validateStartedReceipt(t *testing.T, c *fixtureCase) {
	t.Helper()
	var started task.ProviderStartedRecord
	if err := task.DecodeStrict(readTestFile(t, c.path("provider.started.json")), &started); err != nil {
		t.Fatal(err)
	}
	if err := task.ValidateProviderStartedRecord(&started); err != nil {
		t.Fatal(err)
	}
	var request task.TaskRecord
	requestBytes, metaBytes := readTestFile(t, c.path("task.json")), readTestFile(t, c.path("meta.json"))
	if err := task.DecodeStrict(requestBytes, &request); err != nil {
		t.Fatal(err)
	}
	if started.RootID != request.RootID || started.TaskID != c.id || started.SpecSHA256 != task.ComputeSHA256(requestBytes) || started.MetaSHA256 != task.ComputeSHA256(metaBytes) {
		t.Fatalf("started receipt does not bind the immutable task: %+v", started)
	}
}

func assertStartedTrace(t *testing.T, events, point string) {
	t.Helper()
	steps := []string{"after-start-sink:.", "write:provider.started.json"}
	if point != "fail-started-write" {
		steps = append(steps, "file-barrier:provider.started.json", "close:provider.started.json", "link:provider.started.json", "before-started-link:.")
	}
	if point == "after-started-barrier" || point == "fail-started-barrier" {
		steps = append(steps, "after-started-link-before-barrier:.", "barrier:provider.started.json")
	}
	if point == "after-started-barrier" {
		steps = append(steps, "barrier-complete:provider.started.json", "after-started-barrier:.")
	}
	assertEventOrder(t, events, steps...)
	if point == "fail-started-write" && strings.Contains(string(readTestFile(t, events)), ":link:provider.started.json\n") {
		t.Fatal("started write failure reached link")
	}
}

func assertStartedInterruption(t *testing.T, c *fixtureCase) {
	t.Helper()
	assertAbsent(t, c.path("provider.exit"))
	assertAbsent(t, c.path("outcome.json"))
	for _, name := range []string{"raw/stdout", "raw/stderr"} {
		if len(readTestFile(t, c.path(name))) != 0 {
			t.Fatalf("started checkpoint reached later raw capture: %s", name)
		}
	}
	before := capturePresent(t, c.path("provider.start"), c.path("provider.started.json"), c.path("raw/stdout"), c.path("raw/stderr"), c.sink)
	c.assertSink(t, "runner_sink", 1)
	c.run(t, 11, "claim", "-kind", "runner", "-fake-sink-event", c.sink)
	c.run(t, 14, "collect")
	c.inspect(t, task.PublicationUnknown)
	for path, saved := range before {
		assertSnapshot(t, path, saved)
	}
	assertAbsent(t, c.path("provider.exit"))
	assertAbsent(t, c.path("outcome.json"))
}
