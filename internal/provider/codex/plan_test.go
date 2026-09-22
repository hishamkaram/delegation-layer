package codex

import (
	"errors"
	"slices"
	"strings"
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

func TestExecArgumentsUseNativePermissionsBeforeResume(t *testing.T) {
	request := profileRequest()
	want := []string{
		"exec", "--json", "--color", "never", "--sandbox", "read-only", "-c", `approval_policy="never"`,
		"--cd", request.CanonicalCwd, "--output-last-message", "", "-",
	}
	args, output, err := execArguments(request)
	if err != nil || !slices.Equal(args, want) || output != 11 {
		t.Fatalf("fresh argv=%q output=%d err=%v", args, output, err)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "0199a213-81c0-7800-8aa1-bbab2a035a53", PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	want = append(want[:len(want)-1], "resume", request.PriorSession.ConversationID, "-")
	args, output, err = execArguments(request)
	if err != nil || !slices.Equal(args, want) || output != 11 {
		t.Fatalf("resume argv=%q output=%d err=%v", args, output, err)
	}
}

func TestExecArgumentsPassModelAndEffortWithNativeEncoding(t *testing.T) {
	request := profileRequest()
	request.RequestedConfig.Model = "gpt-5.6"
	request.RequestedConfig.Effort = `high "mode"`
	want := []string{
		"exec", "--json", "--color", "never", "--sandbox", "read-only", "-c", `approval_policy="never"`,
		"--model", "gpt-5.6", "-c", `model_reasoning_effort="high \"mode\""`,
		"--cd", request.CanonicalCwd, "--output-last-message", "", "-",
	}
	args, output, err := execArguments(request)
	if err != nil || !slices.Equal(args, want) || output != 15 {
		t.Fatalf("argv=%q output=%d err=%v", args, output, err)
	}
}

func TestExecArgumentsDoNotInjectRemovedConfigOverrides(t *testing.T) {
	args, _, err := execArguments(profileRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{
		"--ignore-user-config", "--ignore-rules", "--strict-config",
		"features.shell_snapshot=false", "features.shell_snapshot_v2=false",
		"features.apps=false", "features.hooks=false", "features.plugins=false",
		`cli_auth_credentials_store="file"`,
	} {
		if slices.Contains(args, removed) {
			t.Fatalf("native Codex argv retained removed override %q: %q", removed, args)
		}
	}
}

func TestLegacyExecArgumentsRetainHistoricalControls(t *testing.T) {
	args, output, err := legacyExecArguments(profileRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, historical := range []string{"--ignore-user-config", "--ignore-rules", "--strict-config", "features.shell_snapshot_v2=false"} {
		if !slices.Contains(args, historical) {
			t.Fatalf("historical Codex argv lost %q: %q", historical, args)
		}
	}
	if output != 30 {
		t.Fatalf("historical output index=%d want 30", output)
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
	if args[5] != "workspace-write" || output != 11 {
		t.Fatalf("workspace-write argv=%q output=%d", args, output)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "0199a213-81c0-7800-8aa1-bbab2a035a53", PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	resumed, _, err := execArguments(request)
	if err != nil || !slices.Contains(resumed, "resume") || !slices.Contains(resumed, request.PriorSession.ConversationID) || resumed[5] != "workspace-write" {
		t.Fatalf("workspace-write continuation argv=%q err=%v", resumed, err)
	}
}

func TestRuntimeRequirementsOmitRemovedCodexControls(t *testing.T) {
	requirements := RuntimeRequirements()
	for _, removed := range []string{"--ignore-user-config", "--ignore-rules", "--strict-config"} {
		if slices.Contains(requirements.RequiredFlags, removed) {
			t.Fatalf("runtime requirements retain removed flag %q", removed)
		}
	}
}

func TestRuntimeRequirementsAddRequestedModelOnly(t *testing.T) {
	base := runtimeRequirementsForRequest(profileRequest(), RuntimeRequirements())
	if slices.Contains(base.RequiredFlags, "--model") {
		t.Fatalf("default runtime requirements contain model flag: %q", base.RequiredFlags)
	}
	request := profileRequest()
	request.RequestedConfig.Model = "gpt-5.6"
	request.RequestedConfig.Effort = "high"
	got := runtimeRequirementsForRequest(request, RuntimeRequirements())
	if !slices.Contains(got.RequiredFlags, "--model") || slices.Contains(got.RequiredFlags, "--effort") {
		t.Fatalf("requested Codex runtime requirements=%q", got.RequiredFlags)
	}
}

func TestLegacyRuntimeRequirementsPreserveSnapshotFlagOrder(t *testing.T) {
	requirements := legacyRuntimeRequirements()
	want := []string{
		"-c", "--strict-config", "--sandbox", "--cd", "--ignore-user-config", "--ignore-rules",
		"--output-last-message", "--json", "--color",
	}
	if !slices.Equal(requirements.HelpArgs, []string{"exec"}) || !slices.Equal(requirements.RequiredFlags, want) {
		t.Fatalf("legacy runtime requirements=%+v", requirements)
	}
}

func TestExecArgumentsRejectUnsupportedRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"native timeout":     func(r *task.TaskRecord) { r.RequestedConfig.NativeTimeout = "3s" },
		"model newline":      func(r *task.TaskRecord) { r.RequestedConfig.Model = "model\nname" },
		"effort tab":         func(r *task.TaskRecord) { r.RequestedConfig.Effort = "high\t" },
		"oversized model":    func(r *task.TaskRecord) { r.RequestedConfig.Model = strings.Repeat("m", task.MaxControlRecordSize+1) },
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
