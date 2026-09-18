package claude

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func profileRequest() task.TaskRecord {
	return task.TaskRecord{
		RootID: "fedcba9876543210fedcba9876543210", TaskID: "0123456789abcdef0123456789abcdef",
		Provider: Provider, Mode: Mode, CanonicalCwd: "/workspace with spaces",
		BriefLength: 12, BudgetNanos: int64(120 * time.Second),
		RequestedConfig: task.TaskConfig{Permission: Mode, Effort: "default"},
	}
}

func TestFinalizePreparedProfileUsesPortableRuntimeFacts(t *testing.T) {
	request := profileRequest()
	arguments, inputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	cli := commonprovider.CLIInfo{Path: "/usr/local/bin/claude", SHA256: strings.Repeat("a", 64)}
	environment := profileEnvironment{
		RuntimeSHA256: cli.SHA256,
		WritableRoots: []string{"/home/test/.claude"},
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: cli.Path,
		Version:    "Claude Code 99.7.3",
		SHA256:     cli.SHA256,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := finalizePreparedProfile(request, arguments, inputs, cli, environment, nil, strings.Repeat("b", 64), json.RawMessage(facts), time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ObservedVersion != "Claude Code 99.7.3" || prepared.Effective.Policy == nil {
		t.Fatalf("portable runtime facts were not finalized: %+v", prepared)
	}
}

func TestPrintArgumentsPreserveContainmentAndTaskOwnedFilesOnResume(t *testing.T) {
	request := profileRequest()
	args, inputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--print", "--input-format", "text", "--output-format", "stream-json", "--verbose",
		"--safe-mode", "--restricted", "--tools", "Read,Glob,Grep", "--disallowedTools", "mcp__*",
		"--strict-mcp-config", "--mcp-config", "", "--settings", "", "--permission-mode", "dontAsk",
		"--permission-prompts", "none", "--disable-slash-commands", "--no-chrome",
	}
	fresh, err := FreshSessionID(request.RootID, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, append(slices.Clone(want), "--session-id", fresh)) {
		t.Fatalf("fresh argv=%q", args)
	}
	normalized, err := task.NormalizeInputFiles(inputs)
	if err != nil || len(normalized) != 2 {
		t.Fatalf("config declarations=%+v err=%v", inputs, err)
	}
	for _, input := range inputs {
		if args[input.ArgumentIndex] != "" {
			t.Fatal("configuration path is not a reserved task-owned slot")
		}
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: fresh, PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	resumed, resumeInputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resumed, append(slices.Clone(want), "--resume", fresh)) || !slices.Equal(inputs, resumeInputs) {
		t.Fatalf("resume changed restriction flags or input declarations: %q %+v", resumed, resumeInputs)
	}
}

func TestPrintArgumentsUseNativeWorkspaceWritePermission(t *testing.T) {
	request := profileRequest()
	request.Mode = WorkspaceWriteMode
	request.RequestedConfig.Permission = WorkspaceWriteMode
	args, inputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "acceptEdits") || !slices.Contains(args, "Read,Edit,Write,Glob,Grep") {
		t.Fatalf("workspace-write argv=%q", args)
	}
	if len(inputs) != 2 || inputs[0].Content != workspaceWriteProfileSettings {
		t.Fatalf("workspace-write profile inputs=%+v", inputs)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "123e4567-e89b-12d3-a456-426614174000", PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	resumed, _, err := printArguments(request)
	if err != nil || !slices.Contains(resumed, "--resume") || !slices.Contains(resumed, request.PriorSession.ConversationID) {
		t.Fatalf("workspace-write continuation argv=%q err=%v", resumed, err)
	}
}

func TestPrintArgumentsRejectUnsupportedRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"provider":          func(r *task.TaskRecord) { r.Provider = "codex:exec" },
		"mode":              func(r *task.TaskRecord) { r.Mode = "workspace-write" },
		"model":             func(r *task.TaskRecord) { r.RequestedConfig.Model = "other" },
		"effort":            func(r *task.TaskRecord) { r.RequestedConfig.Effort = "max" },
		"timeout":           func(r *task.TaskRecord) { r.RequestedConfig.NativeTimeout = "3s" },
		"budget":            func(r *task.TaskRecord) { r.BudgetNanos = 0 },
		"brief":             func(r *task.TaskRecord) { r.BriefLength = 0 },
		"oversized brief":   func(r *task.TaskRecord) { r.BriefLength = task.MaxBriefSize + 1 },
		"workspace":         func(r *task.TaskRecord) { r.CanonicalCwd = "relative" },
		"unclean workspace": func(r *task.TaskRecord) { r.CanonicalCwd = "/workspace/../elsewhere" },
		"task ID":           func(r *task.TaskRecord) { r.TaskID = "invalid" },
		"root ID":           func(r *task.TaskRecord) { r.RootID = "invalid" },
		"session alias": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "latest"}
		},
		"foreign session": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: "codex:exec", ConversationID: "d3d3d3d3-d3d3-53d3-a3d3-d3d3d3d3d3d3", PredecessorTaskID: r.TaskID}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := profileRequest()
			mutate(&request)
			if _, _, err := printArguments(request); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
