package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestScheduleArgumentsPreserveRequestedValues(t *testing.T) {
	root := t.TempDir()
	brief := filepath.Join(root, "brief")
	args, err := ParseArguments([]string{"--root", root, "schedule", "create", "--brief", brief, "--cwd", root, "--auto", "--cron", "0 9 * * *", "--timezone=", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if args.ScheduleAction != "create" || args.ScheduleCron != "0 9 * * *" || !args.ScheduleTimezoneSet || args.ScheduleTimezone != "" || !args.Auto || !args.JSON || args.Config.Permission != "" {
		t.Fatalf("unexpected requested schedule arguments: %+v", args)
	}
	args, err = ParseArguments([]string{"schedule", "create", "--cron", "0 9 * * *"})
	if err != nil || args.ScheduleTimezoneSet {
		t.Fatalf("omitted timezone: args=%+v err=%v", args, err)
	}
}

func TestScheduleArgumentsActionsAndForbiddenFlags(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, action := range []string{"show", "pause", "resume", "delete"} {
		t.Run(action, func(t *testing.T) {
			args, err := ParseArguments([]string{"schedule", "--json", action, id})
			if err != nil || args.ScheduleID != id || !args.JSON {
				t.Fatalf("args=%+v err=%v", args, err)
			}
		})
	}
	args, err := ParseArguments([]string{"schedule", "list"})
	if err != nil || args.ScheduleLimit != 50 {
		t.Fatalf("default page: args=%+v err=%v", args, err)
	}
	for _, option := range []string{"--interactive", "--resume-task", "--workspace", "--runner", "--pueue-config"} {
		t.Run(option, func(t *testing.T) {
			if _, err := ParseArguments([]string{"schedule", "create", option, "value"}); err == nil {
				t.Fatalf("accepted %s", option)
			}
		})
	}
}
