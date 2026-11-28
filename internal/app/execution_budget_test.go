package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

func TestExecutionBudgetMustBeExplicitOnEveryNewTurn(t *testing.T) {
	cases := [][]string{
		{"dispatch", "--provider", "codex:exec", "--cwd", "/workspace", "--brief", "/brief"},
		{"dispatch", "--provider", "codex:review", "--review-target", "uncommitted", "--cwd", "/workspace", "--brief", "/brief"},
		{"preflight", "--provider", "codex:exec", "--cwd", "/workspace"},
		{"continue", "--task", strings.Repeat("a", 32)},
		{"schedule", "create", "--provider", "codex:exec", "--cwd", "/workspace", "--brief", "/brief", "--cron", "* * * * *"},
	}
	for _, argv := range cases {
		if _, err := ParseArguments(argv); !errors.Is(err, config.ErrBudgetRequired) {
			t.Fatalf("%v: omitted budget returned %v", argv, err)
		}
		for _, budget := range []string{"30m", "unlimited"} {
			request := append(append([]string{}, argv...), "--budget", budget)
			parsed, err := ParseArguments(request)
			if err != nil || parsed.Config.Budget != budget {
				t.Fatalf("%v: explicit budget changed or failed: %v", request, err)
			}
		}
	}
}

func TestContinuationGuidanceRequiresCallerBudgetChoice(t *testing.T) {
	command := continuationCommand("/tmp/state", strings.Repeat("a", 32), "", nil)
	_, suffix, ok := strings.Cut(command, " continue ")
	if !ok || !strings.Contains(suffix, "--budget BUDGET") {
		t.Fatalf("continuation omitted explicit caller choice: %q", command)
	}
	if _, err := ParseArguments(append([]string{"continue"}, strings.Fields(suffix)...)); !errors.Is(err, config.ErrInvalidBudget) {
		t.Fatalf("placeholder supplied execution authority: %v", err)
	}
	for _, budget := range []string{"3m", "unlimited"} {
		argv := strings.Fields(strings.Replace(suffix, "BUDGET", budget, 1))
		parsed, err := ParseArguments(append([]string{"continue"}, argv...))
		if err != nil || parsed.Config.Budget != budget {
			t.Fatalf("chosen budget %q: %+v %v", budget, parsed.Config, err)
		}
	}
}
