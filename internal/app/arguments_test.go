package app

import (
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

func TestInterspersedReadArguments(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, argv := range [][]string{
		{"--root", "/state path", "collect", id, "--watch", "2s", "--json"},
		{"collect", "--json", id, "--root=/state path", "--watch=2s"},
		{"collect", "--json", "--watch=2s", "--root=/state path", "--", id},
	} {
		a, err := ParseArguments(argv)
		if err != nil {
			t.Fatal(err)
		}
		if a.TaskID != id || a.Root != "/state path" || a.Watch != 2*time.Second || !a.JSON {
			t.Fatalf("literal arguments changed: %+v", a)
		}
	}
}

func TestDispatchDefaultsAndLiteralValues(t *testing.T) {
	a, err := ParseArguments([]string{
		"dispatch", "--provider", "codex:exec", "--brief", "brief ; $(inert).md",
		"--cwd", "/workspace ' ; literal", "--model", "--literal-model", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	budget, err := config.ParseBudget(a.Config.Budget)
	if err != nil {
		t.Fatal(err)
	}
	if budget != 30*time.Minute || a.Config.Permission != "read-only" || a.Watch != 0 {
		t.Fatalf("wrong independent budget/watch defaults: %+v", a)
	}
	if a.Brief != "brief ; $(inert).md" || a.Cwd != "/workspace ' ; literal" || a.Config.Model != "--literal-model" {
		t.Fatalf("argument reparsed: %+v", a)
	}
}

func TestAutoPreflightAndContinueArguments(t *testing.T) {
	auto, err := ParseArguments([]string{
		"dispatch", "--auto", "--brief", "/brief", "--cwd", "/workspace", "--json",
	})
	if err != nil || !auto.Auto || auto.Provider != "" {
		t.Fatalf("automatic dispatch arguments: %+v %v", auto, err)
	}
	preflight, err := ParseArguments([]string{
		"preflight", "--provider", "example:run", "--cwd", "/workspace", "--budget", "2m", "--json",
	})
	if err != nil || preflight.Provider != "example:run" || preflight.Config.Budget != "2m" {
		t.Fatalf("preflight arguments: %+v %v", preflight, err)
	}
	id := strings.Repeat("c", 32)
	continuation, err := ParseArguments([]string{
		"continue", "--task", id, "--brief", "/follow-up", "--budget", "5m", "--json",
	})
	if err != nil || continuation.TaskID != id || continuation.Brief != "/follow-up" || continuation.Config.Budget != "5m" {
		t.Fatalf("continuation arguments: %+v %v", continuation, err)
	}
}

func TestRejectedArguments(t *testing.T) {
	id := strings.Repeat("b", 32)
	base := []string{"dispatch", "--provider", "codex:exec", "--brief", "brief", "--cwd", "/workspace"}
	tests := []struct {
		name string
		argv []string
	}{
		{"unknown command", []string{"unknown"}},
		{"missing ID", []string{"collect", "--json"}},
		{"extra ID", []string{"status", id, id}},
		{"invalid ID", []string{"logs", "ABC"}},
		{"help extra", []string{"help", "unexpected"}},
		{"duplicate JSON", []string{"collect", id, "--json", "--json=false"}},
		{"duplicate global across command", []string{"--root", "/a", "collect", id, "--root", "/a"}},
		{"missing global value", []string{"--root"}},
		{"relative root", []string{"collect", id, "--root=relative"}},
		{"watch negative", []string{"collect", id, "--watch=-1ns"}},
		{"watch overflow", []string{"collect", id, "--watch=999999999999h"}},
		{"watch outside collect", []string{"status", id, "--watch=1s"}},
		{"unparsed option after end", []string{"status", "--", id, "--json"}},
	}
	for _, extra := range [][]string{
		{"--budget=0"},
		{"--budget=-1ns"},
		{"--budget=999999999999h"},
		{"--token-budget=10"},
		{"--dollar-budget=10"},
		{"--argv=anything"},
		{"--permission=unsafe"},
		{"--", "raw argument"},
		{"--id", id, "--resume-task", id},
		{"--brief", "second"},
		{"--runner", "relative"},
		{"--auto", "--provider", "codex:exec"},
	} {
		tests = append(tests, struct {
			name string
			argv []string
		}{strings.Join(extra, " "), append(append([]string(nil), base...), extra...)})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if a, err := ParseArguments(tc.argv); err == nil {
				t.Fatalf("invalid input accepted: %+v", a)
			}
		})
	}
}

func TestNonblockingDefaultsAndHelp(t *testing.T) {
	for _, argv := range [][]string{nil, {"--help"}, {"-h"}, {"help"}, {"--version"}, {"version"}} {
		a, err := ParseArguments(argv)
		if err != nil || (a.Command != "help" && a.Command != "version") {
			t.Fatalf("help/version: %+v %v", a, err)
		}
	}
	a, err := ParseArguments([]string{"collect", strings.Repeat("c", 32), "--json"})
	if err != nil || a.Watch != 0 {
		t.Fatalf("default watch is not nonblocking: %+v %v", a, err)
	}
}
