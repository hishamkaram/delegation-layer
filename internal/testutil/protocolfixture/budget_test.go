package main

import (
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// prepare always writes an explicit budget: its own fixture choice when the
// flag is omitted, or the supplied finite or unlimited budget. The saved text
// is the authority and budget_nanos its derived projection.
func TestPrepareWritesExplicitFixtureBudget(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		text  string
		nanos int64
	}{
		{name: "fixture choice", text: "30m0s", nanos: int64(30 * time.Minute)},
		{name: "finite", flags: []string{"-budget", "90s"}, text: "1m30s", nanos: int64(90 * time.Second)},
		{name: "unlimited", flags: []string{"-budget", "unlimited"}, text: "unlimited", nanos: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCase(t)
			c.prepare(t, tc.flags...)
			var request task.TaskRecord
			if err := task.DecodeStrict(readTestFile(t, c.path("task.json")), &request); err != nil {
				t.Fatal(err)
			}
			if request.RequestedConfig.Budget != tc.text || request.BudgetNanos != tc.nanos {
				t.Fatalf("saved budget = %q/%d, want %q/%d", request.RequestedConfig.Budget, request.BudgetNanos, tc.text, tc.nanos)
			}
			if _, err := request.ExecutionBudget(); err != nil {
				t.Fatalf("saved budget is not a valid execution budget: %v", err)
			}
		})
	}
}

// An explicitly empty or malformed budget is a usage error, not a default.
func TestPrepareRejectsMissingOrMalformedBudget(t *testing.T) {
	for _, value := range []string{"", "soon", "0s", "-1m", "Unlimited"} {
		t.Run(value, func(t *testing.T) {
			c := newCase(t)
			c.run(t, 2, "prepare", "-canonical-cwd", c.cwd, "-brief-file", c.brief, "-budget", value)
			assertAbsent(t, c.path("task.json"))
		})
	}
}

// The runner's -budget flag asserts the exact finite budget bound into
// provider.start. A mismatch still conflicts, and an unlimited assertion,
// which PrepareStart cannot express, is refused before any start record.
func TestRunnerBudgetAssertionStaysExactAndFinite(t *testing.T) {
	t.Run("matching finite", func(t *testing.T) {
		c := preparedCase(t)
		c.seal(t, "answer", "-budget", "30m")
		var start task.ProviderStartRecord
		if err := task.DecodeStrict(readTestFile(t, c.path("provider.start")), &start); err != nil {
			t.Fatal(err)
		}
		if start.BudgetNanos != int64(30*time.Minute) || start.BudgetUnlimited {
			t.Fatalf("provider.start budget = %d/%t", start.BudgetNanos, start.BudgetUnlimited)
		}
	})
	for name, value := range map[string]string{"mismatch": "45m", "unlimited": "unlimited", "malformed": "soon"} {
		t.Run(name, func(t *testing.T) {
			c := preparedCase(t)
			want := 2
			if name == "mismatch" {
				want = 1
			}
			c.run(t, want, "seal", "-raw-stdout", "answer", "-fake-sink-event", c.sink, "-budget", value)
			assertAbsent(t, c.path("provider.start"))
		})
	}
}
