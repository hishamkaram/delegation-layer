package opencode

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	testRootID  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTaskID  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testSession = "ses_01J9K7P2E5S8V1Z3B2C4D6F8G0"
)

func profileRequest(mode string) task.TaskRecord {
	return task.TaskRecord{
		RootID: testRootID, TaskID: testTaskID, Provider: Provider, Mode: mode,
		CanonicalCwd: "/workspace with spaces", BriefLength: 12, BudgetNanos: int64(120 * time.Second),
		RequestedConfig: task.TaskConfig{Permission: mode, Effort: "default"},
	}
}

func TestRunArgumentsMapCallerModeAndContinuation(t *testing.T) {
	readOnly := profileRequest(ModeReadOnly)
	got, err := runArguments(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--format", "json", "--dir", readOnly.CanonicalCwd, "--agent", readOnlyAgentName, "--pure"}
	if !slices.Equal(got, want) {
		t.Fatalf("read-only argv=%q want=%q", got, want)
	}

	write := profileRequest(ModeWorkspaceWrite)
	write.RequestedConfig.Model = "anthropic/claude-sonnet"
	write.RequestedConfig.Effort = "high"
	write.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: testTaskID}
	got, err = runArguments(write)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"run", "--format", "json", "--dir", write.CanonicalCwd, "--agent", workspaceWriteAgentName, "--pure", "--model", "anthropic/claude-sonnet", "--variant", "high", "--auto", "--session", testSession}
	if !slices.Equal(got, want) {
		t.Fatalf("workspace-write argv=%q want=%q", got, want)
	}
}

func TestRunArgumentsRejectInvalidRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"provider": func(r *task.TaskRecord) { r.Provider = "other:run" },
		"mode":     func(r *task.TaskRecord) { r.Mode = "unsupported" },
		"permission mismatch": func(r *task.TaskRecord) {
			r.RequestedConfig.Permission = ModeReadOnly
		},
		"native timeout": func(r *task.TaskRecord) { r.RequestedConfig.NativeTimeout = "1s" },
		"empty budget":   func(r *task.TaskRecord) { r.BudgetNanos = 0 },
		"empty brief":    func(r *task.TaskRecord) { r.BriefLength = 0 },
		"relative cwd":   func(r *task.TaskRecord) { r.CanonicalCwd = "relative" },
		"bad model":      func(r *task.TaskRecord) { r.RequestedConfig.Model = "bad\nmodel" },
		"bad effort":     func(r *task.TaskRecord) { r.RequestedConfig.Effort = "bad\teffort" },
		"bad continuation": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "latest", PredecessorTaskID: testTaskID}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := profileRequest(ModeWorkspaceWrite)
			mutate(&request)
			if _, err := runArguments(request); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("error=%v want unsupported profile", err)
			}
		})
	}
}

func TestDescriptionAdvertisesDirectRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable {
		t.Fatalf("description=%+v", description)
	}
	if !slices.Equal(description.SupportedModes, []string{ModeReadOnly, ModeWorkspaceWrite}) {
		t.Fatalf("modes=%q", description.SupportedModes)
	}
	if len(description.Runtime.RequiredFlags) == 0 || len(description.Runtime.HelpArgs) != 1 || description.Runtime.HelpArgs[0] != "run" {
		t.Fatalf("runtime=%+v", description.Runtime)
	}
	registration := Registration()
	if registration.Prepare == nil || len(registration.Interpreters) != 2 {
		t.Fatalf("registration=%+v", registration)
	}
}
