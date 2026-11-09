package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSchedulerArgumentsPreserveGlobalFlagPlacement(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "state with spaces")
	runner := filepath.Join(directory, "custom runner")
	config := filepath.Join(directory, "pueue config.yml")
	for _, tc := range []struct {
		name   string
		argv   []string
		root   string
		runner string
		config string
		json   bool
	}{
		{name: "defaults", argv: []string{"scheduler", "run"}},
		{name: "root before command", argv: []string{"--root", root, "scheduler", "run", "--json"}, root: root, json: true},
		{name: "root before action", argv: []string{"scheduler", "--root", root, "--json", "run"}, root: root, json: true},
		{name: "root after action", argv: []string{"scheduler", "run", "--root=" + root, "--json"}, root: root, json: true},
		{name: "globals before command", argv: []string{"--root", root, "--runner", runner, "--pueue-config", config, "scheduler", "run", "--json"}, root: root, runner: runner, config: config, json: true},
		{name: "globals before action", argv: []string{"scheduler", "--root=" + root, "--runner=" + runner, "--pueue-config=" + config, "--json", "run"}, root: root, runner: runner, config: config, json: true},
		{name: "globals after action", argv: []string{"scheduler", "run", "--root", root, "--runner", runner, "--pueue-config", config, "--json=false"}, root: root, runner: runner, config: config},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseArguments(tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			if got.Command != "scheduler" || got.SchedulerAction != "run" || got.ScheduleAction != "" {
				t.Fatalf("wrong scheduler command: %+v", got)
			}
			if got.Root != tc.root || got.Runner != tc.runner || got.PueueConfig != tc.config || got.JSON != tc.json {
				t.Fatalf("requested global values changed: %+v", got)
			}
		})
	}
}

func TestSchedulerArgumentsRejectInvalidRequests(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"missing action", []string{"scheduler"}, "scheduler requires serve, status, stop, run or migrate"},
		{"missing action with globals", []string{"--root", root, "scheduler", "--json"}, "scheduler requires serve, status, stop, run or migrate"},
		{"invalid action", []string{"scheduler", "start"}, "scheduler requires serve, status, stop, run or migrate"},
		{"supervisor action", []string{"scheduler", "stop-owner"}, "scheduler requires serve, status, stop, run or migrate"},
		{"case sensitive action", []string{"scheduler", "Run"}, "scheduler requires serve, status, stop, run or migrate"},
		{"extra argument", []string{"scheduler", "run", "extra"}, "unexpected extra argument"},
		{"extra action", []string{"scheduler", "run", "run"}, "unexpected extra argument"},
		{"argument after separator", []string{"scheduler", "run", "--", "extra"}, "unexpected extra argument"},
		{"unknown flag", []string{"scheduler", "run", "--unknown"}, "unknown flag"},
		{"provider flag", []string{"scheduler", "run", "--provider", "codex:exec"}, "--provider"},
		{"automatic selection flag", []string{"scheduler", "run", "--auto"}, "--auto"},
		{"model flag", []string{"scheduler", "run", "--model", "model-exact"}, "--model"},
		{"budget flag", []string{"scheduler", "run", "--budget", "1m"}, "--budget"},
		{"cron flag", []string{"scheduler", "run", "--cron", "* * * * *"}, "--cron"},
		{"watch flag", []string{"scheduler", "run", "--watch", "1s"}, "--watch"},
		{"relative root before command", []string{"--root", "relative", "scheduler", "run"}, "absolute"},
		{"relative root after command", []string{"scheduler", "run", "--root=relative"}, "absolute"},
		{"relative runner before command", []string{"--runner", "relative", "scheduler", "run"}, "absolute"},
		{"relative runner after command", []string{"scheduler", "run", "--runner=relative"}, "absolute"},
		{"relative config before command", []string{"--pueue-config", "relative", "scheduler", "run"}, "absolute"},
		{"relative config after command", []string{"scheduler", "run", "--pueue-config=relative"}, "absolute"},
		{"missing root value", []string{"scheduler", "run", "--root"}, "missing value"},
		{"missing runner value", []string{"scheduler", "run", "--runner"}, "missing value"},
		{"missing config value", []string{"scheduler", "run", "--pueue-config"}, "missing value"},
		{"duplicate root", []string{"--root", root, "scheduler", "run", "--root", root}, "duplicate flag"},
		{"duplicate json", []string{"scheduler", "run", "--json", "--json=false"}, "duplicate flag"},
		{"invalid json", []string{"scheduler", "run", "--json=invalid"}, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseArguments(tc.argv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseArguments(%q) = %+v, %v; want error containing %q", tc.argv, got, err, tc.want)
			}
		})
	}
}

func TestSchedulerArgumentsPreserveScheduleFlags(t *testing.T) {
	root := t.TempDir()
	brief := filepath.Join(root, "brief.md")
	got, err := ParseArguments([]string{
		"--root", root, "schedule", "create", "--provider", "codex:exec", "--brief", brief, "--cwd", root,
		"--cron", "0 9 * * *", "--timezone", "UTC", "--model", "model-exact", "--effort", "high",
		"--permission", "workspace-write", "--budget", "17m", "--native-timeout", "3m", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != "schedule" || got.ScheduleAction != "create" || got.SchedulerAction != "" || got.Root != root {
		t.Fatalf("schedule command changed: %+v", got)
	}
	if got.Provider != "codex:exec" || got.Auto || got.Brief != brief || got.Cwd != root || !got.JSON {
		t.Fatalf("schedule provider request changed: %+v", got)
	}
	if got.ScheduleCron != "0 9 * * *" || got.ScheduleTimezone != "UTC" || !got.ScheduleTimezoneSet {
		t.Fatalf("schedule recurrence changed: %+v", got)
	}
	requireSchedulerRequestedConfig(t, got)
}

func TestSchedulerArgumentsPreserveProviderFlags(t *testing.T) {
	root := t.TempDir()
	runner, config := filepath.Join(root, "runner"), filepath.Join(root, "pueue.yml")
	for _, command := range []string{"dispatch", "preflight"} {
		t.Run(command, func(t *testing.T) {
			argv := []string{
				"--root", root, command, "--provider", "codex:exec", "--cwd", root,
				"--runner", runner, "--pueue-config", config, "--model", "model-exact", "--effort", "high",
				"--permission", "workspace-write", "--budget", "17m", "--native-timeout", "3m", "--json",
			}
			if command == "dispatch" {
				argv = append(argv, "--brief", filepath.Join(root, "brief.md"))
			}
			got, err := ParseArguments(argv)
			if err != nil {
				t.Fatal(err)
			}
			if got.Command != command || got.SchedulerAction != "" || got.ScheduleAction != "" || got.Provider != "codex:exec" || got.Auto {
				t.Fatalf("provider command changed: %+v", got)
			}
			if got.Root != root || got.Cwd != root || got.Runner != runner || got.PueueConfig != config || !got.JSON {
				t.Fatalf("provider global values changed: %+v", got)
			}
			requireSchedulerRequestedConfig(t, got)
		})
	}
}

func requireSchedulerRequestedConfig(t *testing.T, got Arguments) {
	t.Helper()
	if got.Config.Model != "model-exact" || got.Config.Effort != "high" || got.Config.Permission != "workspace-write" || got.Config.Budget != "17m" || got.Config.NativeTimeout != "3m" {
		t.Fatalf("requested execution values changed: %+v", got.Config)
	}
}

func TestSchedulerMigrationArguments(t *testing.T) {
	root := t.TempDir()
	for _, argv := range [][]string{{"scheduler", "migrate", "--root", root, "--json"}, {"--root", root, "scheduler", "migrate", "--json"}} {
		got, err := ParseArguments(argv)
		if err != nil {
			t.Fatal(err)
		}
		if got.Command != "scheduler" || got.SchedulerAction != "migrate" || got.Root != root || !got.JSON {
			t.Fatalf("migration arguments changed: %+v", got)
		}
	}
}
