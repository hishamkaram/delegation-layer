package app

import "testing"

func TestDispatchParsesIndependentNativeTimeout(t *testing.T) {
	parsed, err := ParseArguments([]string{"dispatch", "--provider", "antigravity:print", "--brief", "/brief", "--cwd", "/workspace", "--permission", "workspace-write", "--budget", "2m", "--native-timeout", "3s", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Config.NativeTimeout != "3s" || parsed.Config.Budget != "2m" {
		t.Fatalf("timeout flags changed: %+v", parsed.Config)
	}
}
