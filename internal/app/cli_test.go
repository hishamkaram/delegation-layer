package app

import (
	"strings"
	"testing"
	"time"
)

// These cases are independent examples of the accepted grammar, not output
// generated from the command specification under test.
func TestArgumentCompatibilityCorpus(t *testing.T) {
	id := strings.Repeat("a", 32)
	tests := []struct {
		argv                   []string
		command, root, literal string
	}{
		{[]string{"-root", "/state with spaces", "status", id, "-json"}, "status", "/state with spaces", ""},
		{[]string{"dispatch", "--provider", "codex:exec", "--brief", "--help", "--cwd", "/work", "--model", "-json"}, "dispatch", "", "--help"},
		{[]string{"schedule", "-json", "-root=/state", "history", id}, "schedule", "/state", ""},
		{[]string{"teams", "--root", "/state", "--json", "list"}, "teams", "/state", ""},
		{[]string{"workspace", "exec", id, "--budget", "1m", "--", "printf", "--help", "literal value"}, "workspace", "", ""},
		{[]string{"checkpoint", "--json", "status", id, "--watch", "0s"}, "checkpoint", "", ""},
		{[]string{"--root=/state", "supervisor", "status", "--json"}, "supervisor", "/state", ""},
		{[]string{"wait", "--all", id, "--watch=0s", "--json"}, "wait", "", ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.argv, " "), func(t *testing.T) {
			got, err := ParseArguments(tt.argv)
			if err != nil {
				t.Fatal(err)
			}
			if got.Command != tt.command || got.Root != tt.root || got.Brief != tt.literal {
				t.Fatalf("changed requested values: %+v", got)
			}
			if got.Command == "workspace" && strings.Join(got.ExecArguments, "|") != "printf|--help|literal value" {
				t.Fatalf("command argv changed: %q", got.ExecArguments)
			}
		})
	}
}

func TestCommandHelpRoutes(t *testing.T) {
	for _, argv := range [][]string{{"dispatch", "--help"}, {"schedule", "--help"}, {"schedule", "create", "--help"}, {"teams", "default", "set", "-h"}, {"help", "schedule", "policy", "set"}} {
		got, err := ParseArguments(argv)
		if err != nil || got.Command != "help" {
			t.Errorf("%v: command=%q err=%v", argv, got.Command, err)
		}
	}
}

func FuzzArgumentLiteralValues(f *testing.F) {
	for _, value := range []string{"--help", "-json", "space and quotes ' ; $(literal)", "status", "--", ""} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, err := ParseArguments([]string{"dispatch", "--provider", "codex:exec", "--brief", "/brief", "--cwd", "/workspace", "--model", value})
		if err != nil {
			t.Fatalf("literal model rejected: %v", err)
		}
		if got.Command != "dispatch" || got.Config.Model != value {
			t.Fatalf("literal %q reinterpreted: %+v", value, got)
		}
	})
}

func TestScopedHelpUsesRegisteredFlagsAndDefaults(t *testing.T) {
	help, err := HelpText([]string{"wait"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"Usage: delegate wait TASK_ID...", "--watch", "5m0s", "--any", "--all", "3 finite watch"} {
		if !strings.Contains(help, fragment) {
			t.Errorf("missing %q in help:\n%s", fragment, help)
		}
	}
	if strings.Contains(help, "--provider") {
		t.Fatal("wait help advertises a dispatch-only flag")
	}
	root, err := HelpText(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"tui", "--root", "--pueue-config", "--runner"} {
		if !strings.Contains(root, fragment) {
			t.Errorf("root help missing %q", fragment)
		}
	}
	scheduler, err := HelpText([]string{"scheduler", "run"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scheduler, "scheduler-host-read-fd") {
		t.Fatal("private descriptor flag exposed in public help")
	}
}

func TestHelpLexingPreservesValuesAndRejectsMalformedInput(t *testing.T) {
	for _, argv := range [][]string{
		{"schedule", "--help", "--help"},
		{"schedule", "--json=invalid", "--help"},
		{"dispatch", "--unknown", "--help"},
		{"collect", "--", "--help"},
		{"help", "unknown"},
		{"help", "schedule", "unknown"},
	} {
		if got, err := ParseArguments(argv); err == nil {
			t.Errorf("invalid help syntax %v accepted: %+v", argv, got)
		}
	}
	got, err := ParseArguments([]string{"dispatch", "--provider", "codex:exec", "--cwd", "/work", "--brief", "--help"})
	if err != nil || got.Command != "dispatch" || got.Brief != "--help" {
		t.Fatalf("literal value became help: %+v %v", got, err)
	}
	got, err = ParseArguments([]string{"dispatch", "--budget", "invalid", "--help"})
	if err != nil || got.Command != "help" {
		t.Fatalf("help incorrectly requires semantic inputs: %+v %v", got, err)
	}
}

func TestCommandDefaultsComeFromRegistration(t *testing.T) {
	wait, err := CommandDefaults([]string{"wait"})
	if err != nil {
		t.Fatal(err)
	}
	if wait.Watch != 5*time.Minute || wait.WaitAll {
		t.Fatalf("wait defaults=%+v", wait)
	}
	schedule, err := CommandDefaults([]string{"schedule", "create"})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ScheduleHook == nil || schedule.ScheduleHook.Threshold != 1 || schedule.ScheduleHook.Timeout != 10*time.Second {
		t.Fatalf("schedule defaults=%+v", schedule)
	}
	schedule.ScheduleHook.Threshold = 42
	second, err := CommandDefaults([]string{"schedule", "create"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ScheduleHook.Threshold != 1 {
		t.Fatal("request defaults share mutable hook state")
	}
	for _, path := range [][]string{nil, {"schedule"}, {"unknown"}, {"schedule", "missing"}} {
		if _, err := CommandDefaults(path); err == nil {
			t.Errorf("incomplete path %q accepted", path)
		}
	}
}

func TestProgrammaticContentSourcesAreExclusive(t *testing.T) {
	for _, command := range []string{"dispatch", "steer", "reply"} {
		args := Arguments{Command: command, Provider: "codex:exec", Cwd: "/work", BriefBytes: []byte("prompt"), MessageID: strings.Repeat("a", 32), RequestID: strings.Repeat("b", 32)}
		if command == "reply" {
			args.AnswersBytes = []byte(`{"answers":[]}`)
		}
		var err error
		if command == "dispatch" {
			args.Config.Permission = "read-only"
			err = args.validateDispatch(nil)
		} else {
			err = args.validateInteractionArguments()
		}
		if err != nil {
			t.Errorf("%s inline content refused: %v", command, err)
		}
		args.Brief = "/path"
		if command == "reply" {
			args.Answers = "/path"
		}
		if command == "dispatch" {
			err = args.validateDispatch(nil)
		} else {
			err = args.validateInteractionArguments()
		}
		if err == nil {
			t.Errorf("%s accepted conflicting content sources", command)
		}
	}
}

func TestLegacyGroupSeparatorPlacement(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, argv := range [][]string{{"supervisor", "--", "status"}, {"scheduler", "--", "status"}, {"workspace", "--", "status", id}, {"checkpoint", "--", "status", id}, {"devices", "--", "list"}, {"remote", "--", "list"}} {
		if _, err := ParseArguments(argv); err != nil {
			t.Errorf("legacy accepted separator placement %v rejected: %v", argv, err)
		}
	}
}

func TestScopedHelpStatesRequiredSelectors(t *testing.T) {
	for _, fixture := range []struct {
		path     []string
		required []string
	}{
		{[]string{"models"}, []string{"Required: --provider"}},
		{[]string{"capabilities"}, []string{"Required: --provider"}},
		{[]string{"teams", "show"}, []string{"Required: --scope", "--team"}},
		{[]string{"teams", "update"}, []string{"Required: --scope", "--team", "--definition", "--expected-revision"}},
		{[]string{"devices", "probe"}, []string{"Required: --device"}},
		{[]string{"devices", "trust"}, []string{"Required: --device", "--fingerprint"}},
	} {
		help, err := HelpText(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range append(append([]string(nil), fixture.required...), "Example:") {
			if !strings.Contains(help, required) {
				t.Errorf("%v missing %q", fixture.path, required)
			}
		}
	}
}

func TestAuthoredCommandExamplesPassSemanticParsing(t *testing.T) {
	if walkCommandExamples(t, commandTree(), nil) == 0 {
		t.Fatal("no authored command examples were exercised")
	}
}

func walkCommandExamples(t *testing.T, spec commandSpec, path []string) int {
	t.Helper()
	examples := 0
	if len(spec.Example) > 0 {
		examples++
		t.Run(strings.Join(path, "/"), func(t *testing.T) { assertCommandExample(t, spec.Example, path) })
	}
	for _, child := range spec.Children {
		examples += walkCommandExamples(t, child, append(append([]string(nil), path...), child.Name))
	}
	return examples
}

func assertCommandExample(t *testing.T, example, path []string) {
	t.Helper()
	if example[0] != "delegate" || len(example) < len(path)+1 {
		t.Fatalf("invalid example executable/path: %q", example)
	}
	for i, name := range path {
		if example[i+1] != name {
			t.Fatalf("example targets another command: %q", example)
		}
	}
	argv := exampleFixtureIDs(example)
	parsed, err := ParseArguments(argv[1:])
	if err != nil || parsed.Command == "help" {
		t.Fatalf("authored example is not a valid operation: %q -> %+v %v", argv, parsed, err)
	}
}

func exampleFixtureIDs(example []string) []string {
	// Substitute symbolic domain identifiers only. Missing flags, selectors,
	// operands and invalid values must be caught by real semantic parsing.
	values := map[string]string{
		"TASK_ID": strings.Repeat("a", 32), "MESSAGE_ID": strings.Repeat("b", 32),
		"SCHEDULE_ID": strings.Repeat("c", 32), "WORKSPACE_ID": strings.Repeat("d", 32),
		"CHECKPOINT_ID": strings.Repeat("e", 32), "TEAM_ID": "review", "REQUEST_ID": "request-1",
	}
	argv := append([]string(nil), example...)
	for i, value := range argv {
		if fixture, ok := values[value]; ok {
			argv[i] = fixture
		}
	}
	return argv
}

func TestHelpPendingExitsKeepDistinctOwnerMeanings(t *testing.T) {
	// Owner sites: scheduling.savedScheduleHostResult; infrastructure.stopOwner;
	// interaction.events; checkpointAcknowledgement; workspace pending projection.
	for _, fixture := range []struct {
		path  []string
		facts []string
	}{
		{[]string{"schedule", "create"}, []string{"Common exit codes:", "3 host readiness", "saved is true", "do not repeat creation"}},
		{[]string{"schedule", "resume"}, []string{"3 host readiness", "resume is already saved"}},
		{[]string{"supervisor", "stop-owner"}, []string{"3 owner is busy", "stop was refused"}},
		{[]string{"events"}, []string{"3 finite watch found no new events"}},
		{[]string{"workspace", "exec"}, []string{"3 workspace operation pending", "acknowledged operation"}},
		{[]string{"checkpoint", "create"}, []string{"3 no final creation receipt", "reason_code", "next_action"}},
		{[]string{"dispatch"}, []string{"3 managed workspace admission pending", "remote acceptance/connectivity uncertain"}},
		{[]string{"continue"}, []string{"3 managed workspace preparation pending"}},
		{[]string{"recheck"}, []string{"4 evidence remains rejected"}},
		{[]string{"status"}, []string{"4 returned remote task outcome is rejected"}},
		{[]string{"remote", "reconcile"}, []string{"4 returned remote task outcome is rejected"}},
	} {
		help, err := HelpText(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range fixture.facts {
			if !strings.Contains(help, fact) {
				t.Errorf("%v missing operational exit meaning %q", fixture.path, fact)
			}
		}
	}
}
