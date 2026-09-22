package antigravity

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrintArgumentsFreshAndExactResume(t *testing.T) {
	request := argumentRequest()
	want := []string{"--sandbox", "--mode", "accept-edits", "--add-dir", request.CanonicalCwd, "--output-format", "json", "--input-format", "text", "--disable-slash-commands", "--print-timeout", "2m0s"}
	got, err := printArguments(request)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("fresh arguments = %q, %v", got, err)
	}
	request.RequestedConfig.Effort = "default"
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "b4fb555c-9c20-4aba-b048-6ef3512a4633"}
	want = append(want, "--conversation", request.PriorSession.ConversationID)
	got, err = printArguments(request)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("resume arguments = %q, %v", got, err)
	}
}

func TestPrintArgumentsReadOnlyUsesNativePlanMode(t *testing.T) {
	request := argumentRequest()
	request.Mode = ModeReadOnly
	request.RequestedConfig.Permission = ModeReadOnly
	want := []string{"--sandbox", "--mode", "plan", "--add-dir", request.CanonicalCwd, "--output-format", "json", "--input-format", "text", "--disable-slash-commands", "--print-timeout", "2m0s"}
	got, err := printArguments(request)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("read-only arguments = %q, %v", got, err)
	}
}

func TestPrintArgumentsPassModelAndEffort(t *testing.T) {
	request := argumentRequest()
	request.RequestedConfig.Model = "gemini-3-pro"
	request.RequestedConfig.Effort = "high"
	want := []string{"--sandbox", "--mode", "accept-edits", "--add-dir", request.CanonicalCwd, "--output-format", "json", "--input-format", "text", "--disable-slash-commands", "--print-timeout", "2m0s", "--model", "gemini-3-pro", "--effort", "high"}
	got, err := printArguments(request)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("model and effort arguments = %q, %v", got, err)
	}
}

func TestRuntimeRequirementsAddOptionalFlagsOnlyWhenRequested(t *testing.T) {
	base := runtimeRequirementsForRequest(argumentRequest())
	if slices.Contains(base.RequiredFlags, "--model") || slices.Contains(base.RequiredFlags, "--effort") {
		t.Fatalf("default runtime requirements contain optional flags: %q", base.RequiredFlags)
	}
	request := argumentRequest()
	request.RequestedConfig.Model = "gemini-3-pro"
	request.RequestedConfig.Effort = "high"
	got := runtimeRequirementsForRequest(request)
	if !slices.Contains(got.RequiredFlags, "--model") || !slices.Contains(got.RequiredFlags, "--effort") {
		t.Fatalf("requested runtime requirements omitted optional flags: %q", got.RequiredFlags)
	}
}

func TestPrintArgumentsRejectUnsupportedRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"missing workspace":   func(r *task.TaskRecord) { r.CanonicalCwd = "" },
		"relative workspace":  func(r *task.TaskRecord) { r.CanonicalCwd = "relative" },
		"unclean workspace":   func(r *task.TaskRecord) { r.CanonicalCwd = "/work/../other" },
		"permission mismatch": func(r *task.TaskRecord) { r.RequestedConfig.Permission = "read-only" },
		"provider mismatch":   func(r *task.TaskRecord) { r.Provider = "codex:exec" },
		"zero budget":         func(r *task.TaskRecord) { r.BudgetNanos = 0 },
		"negative budget":     func(r *task.TaskRecord) { r.BudgetNanos = -1 },
		"model newline":       func(r *task.TaskRecord) { r.RequestedConfig.Model = "some\nmodel" },
		"effort tab":          func(r *task.TaskRecord) { r.RequestedConfig.Effort = "max\t" },
		"oversized model":     func(r *task.TaskRecord) { r.RequestedConfig.Model = strings.Repeat("m", task.MaxControlRecordSize+1) },
		"resume selector": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "--last"}
		},
		"resume provider": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: "codex:exec", ConversationID: "b4fb555c-9c20-4aba-b048-6ef3512a4633"}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			request := argumentRequest()
			change(&request)
			arguments, err := printArguments(request)
			if !errors.Is(err, ErrUnsupportedProfile) || arguments != nil {
				t.Fatalf("unsupported request produced argv %q, error %v", arguments, err)
			}
		})
	}
}

func argumentRequest() task.TaskRecord {
	return task.TaskRecord{Provider: Provider, Mode: Mode, CanonicalCwd: "/workspace with spaces", BudgetNanos: int64(2 * time.Minute), RequestedConfig: task.TaskConfig{Permission: Mode}}
}

func TestPrintArgumentsNativeTimeoutKeepsOuterBudget(t *testing.T) {
	request := argumentRequest()
	request.RequestedConfig.NativeTimeout = "3s"
	arguments, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if arguments[len(arguments)-1] != "3s" || request.BudgetNanos != int64(2*time.Minute) {
		t.Fatalf("native timeout or budget changed unexpectedly: %q %+v", arguments, request)
	}
	for _, value := range []string{"0s", "-1s", "121s", "not-a-duration"} {
		request.RequestedConfig.NativeTimeout = value
		if _, err = printArguments(request); !errors.Is(err, ErrUnsupportedProfile) {
			t.Errorf("invalid timeout %q accepted: %v", value, err)
		}
	}
}
