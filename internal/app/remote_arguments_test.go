package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteDispatchArgumentsSeparateWorkspaces(t *testing.T) {
	brief := filepath.Join(t.TempDir(), "brief.txt")
	args := []string{"dispatch", "--device", "worker", "--provider", "claude:print", "--brief", brief, "--remote-cwd", "/srv/project", "--json"}
	parsed, err := ParseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Device != "worker" || parsed.RemoteCwd != "/srv/project" || parsed.Cwd != "" {
		t.Fatalf("routing lost: %+v", parsed)
	}
	for _, extra := range [][]string{{"--cwd", "/tmp/local"}, {"--auto"}, {"--workspace", strings.Repeat("a", 32)}} {
		if _, err = ParseArguments(append(append([]string{}, args...), extra...)); err == nil {
			t.Fatalf("accepted conflicting %v", extra)
		}
	}
}

func TestRemoteHandleRemainsDistinct(t *testing.T) {
	handle := "remote:" + strings.Repeat("a", 32)
	for _, command := range []string{"status", "collect", "cancel", "events"} {
		parsed, err := ParseArguments([]string{command, handle, "--json"})
		if err != nil || parsed.TaskID != handle {
			t.Fatalf("%s lost handle: %v", command, err)
		}
	}
}

func TestRemoteServeRequiresStdio(t *testing.T) {
	if _, err := ParseArguments([]string{"remote", "serve"}); err == nil {
		t.Fatal("accepted unframed server")
	}
	parsed, err := ParseArguments([]string{"remote", "serve", "--stdio"})
	if err != nil || !parsed.Stdio {
		t.Fatalf("server arguments: %v", err)
	}
}
