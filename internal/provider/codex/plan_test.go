package codex

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func profileRequest() task.TaskRecord {
	return task.TaskRecord{
		TaskID: "0123456789abcdef0123456789abcdef", Provider: Provider, Mode: Mode,
		CanonicalCwd: "/workspace with spaces", BriefLength: 12, BudgetNanos: int64(120 * time.Second),
		RequestedConfig: task.TaskConfig{Permission: Mode, Effort: "default"},
	}
}

func TestExecArgumentsKeepReadOnlyControlsBeforeResume(t *testing.T) {
	request := profileRequest()
	want := []string{
		"exec", "--json", "--color", "never", "--ignore-user-config", "--ignore-rules", "--strict-config",
		"--sandbox", "read-only", "-c", `approval_policy="never"`, "-c", `approvals_reviewer="user"`, "-c", "allow_login_shell=false",
		"-c", "features.shell_snapshot=false", "-c", "features.shell_snapshot_v2=false", "-c", "features.apps=false",
		"-c", "features.hooks=false", "-c", "features.plugins=false", "-c", `cli_auth_credentials_store="file"`,
		"--cd", request.CanonicalCwd, "--output-last-message", "", "-",
	}
	args, output, err := execArguments(request)
	if err != nil || !slices.Equal(args, want) || output != 30 {
		t.Fatalf("fresh argv=%q output=%d err=%v", args, output, err)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "0199a213-81c0-7800-8aa1-bbab2a035a53", PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	want = append(want[:len(want)-1], "resume", request.PriorSession.ConversationID, "-")
	args, output, err = execArguments(request)
	if err != nil || !slices.Equal(args, want) || output != 30 {
		t.Fatalf("resume argv=%q output=%d err=%v", args, output, err)
	}
}

func TestExecArgumentsUseNativeWorkspaceWriteSandbox(t *testing.T) {
	request := profileRequest()
	request.Mode = WorkspaceWriteMode
	request.RequestedConfig.Permission = WorkspaceWriteMode
	args, output, err := execArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if args[8] != "workspace-write" || output != 30 {
		t.Fatalf("workspace-write argv=%q output=%d", args, output)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "0199a213-81c0-7800-8aa1-bbab2a035a53", PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	resumed, _, err := execArguments(request)
	if err != nil || !slices.Contains(resumed, "resume") || !slices.Contains(resumed, request.PriorSession.ConversationID) || resumed[8] != "workspace-write" {
		t.Fatalf("workspace-write continuation argv=%q err=%v", resumed, err)
	}
}

func TestExecArgumentsRejectUnsupportedRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"model":              func(r *task.TaskRecord) { r.RequestedConfig.Model = "arbitrary" },
		"effort":             func(r *task.TaskRecord) { r.RequestedConfig.Effort = "max" },
		"native timeout":     func(r *task.TaskRecord) { r.RequestedConfig.NativeTimeout = "3s" },
		"unbounded":          func(r *task.TaskRecord) { r.BudgetNanos = 0 },
		"oversized":          func(r *task.TaskRecord) { r.BriefLength = task.MaxBriefSize + 1 },
		"empty brief":        func(r *task.TaskRecord) { r.BriefLength = 0 },
		"relative workspace": func(r *task.TaskRecord) { r.CanonicalCwd = "relative" },
		"session name": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "latest"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := profileRequest()
			mutate(&request)
			if _, _, err := execArguments(request); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("got %v, want unsupported profile", err)
			}
		})
	}
}
