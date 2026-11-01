package app

import (
	"path/filepath"
	"strings"
	"testing"
)

type teamsArgumentsCase struct {
	argv   []string
	action string
	sub    string
	scope  string
}

func TestTeamsArgumentsPreserveRequestedValues(t *testing.T) {
	dir := t.TempDir()
	tests := []teamsArgumentsCase{
		{[]string{"register", "--scope", "user", "--definition", "-"}, "register", "", "user"},
		{[]string{"update", "--scope", "project", "--cwd", dir, "--project-root", dir, "--team", "review", "--expected-revision", "3", "--definition", "-"}, "update", "", "project"},
		{[]string{"delete", "--scope", "user", "--team", "review", "--expected-revision=3"}, "delete", "", "user"},
		{[]string{"show", "--scope", "user", "--team", "review"}, "show", "", "user"},
		{[]string{"list", "--limit", "1", "--after", "user:review"}, "list", "", ""},
		{[]string{"default", "show", "--cwd", dir}, "default", "show", ""},
		{[]string{"default", "set", "--cwd", dir, "--team-scope", "user", "--team", "review"}, "default", "set", "user"},
		{[]string{"default", "clear", "--cwd", dir, "--project-root", dir}, "default", "clear", ""},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.argv[:1], " ")+test.sub, func(t *testing.T) {
			argv := append([]string{"--root", dir, "--json"}, test.argv...)
			a, err := parseTeamsArguments(Arguments{Command: "teams"}, argv)
			if err != nil {
				t.Fatal(err)
			}
			requireTeamsParsedArguments(t, a, test, dir)
		})
	}
	list, err := parseTeamsArguments(Arguments{Command: "teams"}, []string{"list"})
	if err != nil || list.TeamLimit != 50 || list.Cwd != "" {
		t.Fatalf("default list must not invent context: %+v, %v", list, err)
	}
}

func requireTeamsParsedArguments(t *testing.T, a Arguments, expected teamsArgumentsCase, dir string) {
	t.Helper()
	if a.Command != "teams" || a.Root != dir || !a.JSON || a.TeamAction != expected.action || a.TeamDefaultAction != expected.sub || a.TeamScope != expected.scope {
		t.Fatalf("unexpected requested values: %+v", a)
	}
	switch expected.action {
	case "update":
		requireTeamsUpdateArguments(t, a, dir)
	case "list":
		if a.TeamLimit != 1 || a.TeamAfter != "user:review" {
			t.Fatalf("list lost values: %+v", a)
		}
	}
}

func requireTeamsUpdateArguments(t *testing.T, a Arguments, dir string) {
	t.Helper()
	if a.TeamExpectedRevision != 3 || a.TeamDefinition != "-" || a.TeamID != "review" || a.Cwd != dir || a.ProjectRoot != dir {
		t.Fatalf("update lost values: %+v", a)
	}
}

func TestTeamsArgumentsRejectInvalidBoundaries(t *testing.T) {
	dir := t.TempDir()
	tests := [][]string{
		nil,
		{"unknown"},
		{"default"},
		{"default", "unknown"},
		{"list", "unexpected"},
		{"list", "--limit", "0"},
		{"list", "--limit", "101"},
		{"list", "--limit", "1", "--limit", "2"},
		{"list", "--scope", "user"},
		{"list", "--after", "not-a-cursor"},
		{"list", "--after", "user:Bad"},
		{"list", "--root", "relative"},
		{"list", "--cwd", "relative"},
		{"list", "--project-root", dir},
		{"list", "--cwd", dir, "--project-root", "relative"},
		{"register", "--definition", "-"},
		{"register", "--scope", "other", "--definition", "-"},
		{"register", "--scope", "user"},
		{"register", "--scope", "project", "--definition", "-"},
		{"show", "--scope", "user"},
		{"show", "--scope", "user", "--team", "Bad"},
		{"show", "--scope", "user", "--team", "review", "--member", "primary"},
		{"show", "--scope", "user", "--team", "review", "--expected-revision", "1"},
		{"update", "--scope", "user", "--team", "review", "--definition", "-"},
		{"delete", "--scope", "user", "--team", "review", "--expected-revision", "-1"},
		{"delete", "--scope", "user", "--team", "review", "--expected-revision", "9223372036854775808"},
		{"default", "show"},
		{"default", "set", "--cwd", dir, "--team", "review"},
		{"default", "set", "--cwd", dir, "--scope", "user", "--team", "review"},
		{"default", "clear", "--cwd", dir, "--team", "review"},
		{"list", "--definition", filepath.Join(dir, "definition.json")},
	}
	for _, argv := range tests {
		if _, err := parseTeamsArguments(Arguments{Command: "teams"}, argv); err == nil {
			t.Errorf("accepted invalid teams argv %v", argv)
		}
	}
}
