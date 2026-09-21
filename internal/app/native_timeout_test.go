package app

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDispatchParsesIndependentNativeTimeout(t *testing.T) {
	parsed, err := ParseArguments([]string{"dispatch", "--provider", "antigravity:print", "--brief", "/brief", "--cwd", "/workspace", "--permission", "workspace-write", "--budget", "2m", "--native-timeout", "3s", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Config.NativeTimeout != "3s" || parsed.Config.Budget != "2m" {
		t.Fatalf("timeout flags changed: %+v", parsed.Config)
	}
}

func TestContinuationBudgetClampsInheritedNativeTimeout(t *testing.T) {
	predecessor := &task.TaskRecord{
		TaskID:          "predecessor",
		Provider:        "antigravity:print",
		CanonicalCwd:    "/workspace",
		RequestedConfig: task.TaskConfig{Budget: "10m", NativeTimeout: "10m"},
	}
	args := buildContinuationArguments(Arguments{Config: task.TaskConfig{Budget: "1m"}}, "/state", predecessor, nil, []byte("follow-up"))
	if args.Config.NativeTimeout != "1m0s" {
		t.Fatalf("inherited native timeout=%q, want clamped budget", args.Config.NativeTimeout)
	}
}
