package pi

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	testRootID = "fedcba9876543210fedcba9876543210"
	testTaskID = "0123456789abcdef0123456789abcdef"
	testUUID   = "123e4567-e89b-12d3-a456-426614174000"
)

func planRequest(mode string) task.TaskRecord {
	return task.TaskRecord{
		RootID: testRootID, TaskID: testTaskID, Provider: Provider, Mode: mode, CanonicalCwd: "/workspace",
		BriefLength: 4, BudgetNanos: int64(time.Minute),
		RequestedConfig: task.TaskConfig{Permission: mode, Effort: "default"},
	}
}

func TestJSONArgumentsMapCallerPermissionAndContinuation(t *testing.T) {
	readOnly := planRequest(ModeReadOnly)
	args, err := printArguments(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--mode", "json", "--tools", readOnlyTools, "--no-extensions", "--offline"}
	if !slices.Equal(args, want) {
		t.Fatalf("read-only argv=%q want=%q", args, want)
	}

	unsupportedWrite := planRequest(config.ModeWorkspaceWrite)
	unsupportedWrite.RequestedConfig.Model = "openai/gpt-5"
	unsupportedWrite.RequestedConfig.Effort = "high"
	unsupportedWrite.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testUUID, PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	if _, err = printArguments(unsupportedWrite); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("workspace-write error=%v want unsupported profile", err)
	}
}

func TestJSONArgumentsRejectUnsupportedRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"provider":            func(r *task.TaskRecord) { r.Provider = config.ProviderCodexExec },
		"permission mismatch": func(r *task.TaskRecord) { r.RequestedConfig.Permission = config.ModeWorkspaceWrite },
		"unknown mode":        func(r *task.TaskRecord) { r.Mode = "prompt"; r.RequestedConfig.Permission = "prompt" },
		"native timeout":      func(r *task.TaskRecord) { r.RequestedConfig.NativeTimeout = "1s" },
		"thinking":            func(r *task.TaskRecord) { r.RequestedConfig.Effort = "turbo" },
		"bad model":           func(r *task.TaskRecord) { r.RequestedConfig.Model = " model" },
		"budget":              func(r *task.TaskRecord) { r.BudgetNanos = 0 },
		"brief":               func(r *task.TaskRecord) { r.BriefLength = 0 },
		"workspace":           func(r *task.TaskRecord) { r.CanonicalCwd = "relative" },
		"task ID":             func(r *task.TaskRecord) { r.TaskID = "bad" },
		"session": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "latest", PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := planRequest(ModeReadOnly)
			mutate(&request)
			if _, err := printArguments(request); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRuntimeRequirementsCoverAllLaunchFlags(t *testing.T) {
	requirements := RuntimeRequirements()
	for _, flag := range []string{"--mode", "--tools", "--model", "--thinking", "--session", "--session-dir", "--no-extensions", "--offline"} {
		if !slices.Contains(requirements.RequiredFlags, flag) {
			t.Fatalf("required flags=%q missing %q", requirements.RequiredFlags, flag)
		}
	}
}
