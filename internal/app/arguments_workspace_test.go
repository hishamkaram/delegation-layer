package app

import (
	"reflect"
	"strings"
	"testing"
)

func TestManagedWorkspaceDispatchArguments(t *testing.T) {
	id := strings.Repeat("a", 32)
	base := []string{"dispatch", "--provider", "codex:exec", "--brief", "/brief"}
	for _, extra := range [][]string{
		{"--cwd", "/source", "--isolation", "git-worktree", "--base", "HEAD", "--workspace-budget", "8m"},
		{"--workspace", id},
	} {
		if _, err := ParseArguments(append(append([]string(nil), base...), extra...)); err != nil {
			t.Fatal(err)
		}
	}
	for _, extra := range [][]string{
		{"--workspace", id, "--cwd", "/source"},
		{"--workspace", id, "--isolation", "git-worktree"},
		{"--cwd", "/source", "--base", "HEAD"},
		{"--cwd", "/source", "--isolation", "git-worktree", "--workspace-budget", "0s"},
	} {
		if _, err := ParseArguments(append(append([]string(nil), base...), extra...)); err == nil {
			t.Fatalf("accepted conflicting workspace request: %v", extra)
		}
	}
}

func TestWorkspaceExecKeepsLiteralCommandArguments(t *testing.T) {
	id := strings.Repeat("a", 32)
	command := []string{"printf", "--json", "a ; $(literal)", "--budget", "not-a-delegate-option"}
	argv := append([]string{"--root", "/state", "workspace", "exec", id, "--budget", "1m", "--json", "--"}, command...)
	parsed, err := ParseArguments(argv)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.WorkspaceID != id || parsed.WorkspaceAction != "exec" || !reflect.DeepEqual(parsed.ExecArguments, command) {
		t.Fatalf("command arguments were reparsed: %+v", parsed)
	}
}

func TestWorkspaceCommandsRejectMissingCommandOrExtraIDs(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, argv := range [][]string{
		{"workspace", "exec", id},
		{"workspace", "exec", id, "--"},
		{"workspace", "status", id, id},
		{"workspace", "forget", id, "--discard"},
		{"workspace", "release", id, "--budget", "-1s"},
	} {
		if _, err := ParseArguments(argv); err == nil {
			t.Fatalf("accepted invalid command: %v", argv)
		}
	}
}
