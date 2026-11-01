package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestScheduleHistoryArguments(t *testing.T) {
	id := strings.Repeat("b", 32)
	args, err := ParseArguments([]string{"schedule", "history", id, "--json"})
	if err != nil || args.ScheduleAction != "history" || args.ScheduleID != id || args.ScheduleLimit != 50 || !args.JSON {
		t.Fatalf("default history: args=%+v err=%v", args, err)
	}
	before := "2026-10-31T12:34:56.123456789+02:00"
	args, err = ParseArguments([]string{"schedule", "--json", "history", "--before", before, id, "--limit", "100"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := scheduleHistoryPage(args)
	expected := time.Date(2026, 10, 31, 10, 34, 56, 123456789, time.UTC)
	if err != nil || page.Before == nil || !page.Before.Equal(expected) || page.Before.Location() != time.UTC || page.Limit != 100 {
		t.Fatalf("history page=%+v err=%v", page, err)
	}
}

func TestScheduleExecutionArguments(t *testing.T) {
	id := strings.Repeat("b", 32)
	args, err := ParseArguments([]string{"schedule", "execution", id, "--json"})
	if err != nil || args.TaskID != id || args.ScheduleID != "" || args.ScheduleAction != "execution" || !args.JSON {
		t.Fatalf("execution: args=%+v err=%v", args, err)
	}
}

func TestScheduleQueryArgumentsRejectInvalidRequests(t *testing.T) {
	id := strings.Repeat("b", 32)
	for _, argv := range [][]string{
		{"history"},
		{"history", id, id},
		{"execution"},
		{"execution", id, id},
		{"history", id, "--after", id},
		{"execution", id, "--before", "2026-10-31T00:00:00Z"},
		{"execution", id, "--watch", "1s"},
		{"history", id, "--limit", "1", "--limit", "2"},
	} {
		if _, err := ParseArguments(append([]string{"schedule"}, argv...)); err == nil {
			t.Errorf("accepted invalid argv: %v", argv)
		}
	}
	for _, argv := range [][]string{
		{"history", id, "--before", "yesterday"},
		{"history", id, "--before", "0001-01-01T00:00:00Z"},
		{"history", id, "--limit", "0"},
		{"history", id, "--limit", "101"},
		{"history", "invalid"},
		{"execution", "invalid"},
	} {
		args, err := ParseArguments(append([]string{"schedule"}, argv...))
		if err == nil {
			err = validateScheduleRequest(args)
		}
		if err == nil {
			t.Errorf("accepted invalid query: %v", argv)
		}
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
