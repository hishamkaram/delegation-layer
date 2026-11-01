package pi

import (
	"errors"
	"slices"
	"strings"
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
	want := []string{"--mode", "json", "--tools", readOnlyTools, "--no-extensions"}
	if !slices.Equal(args, want) {
		t.Fatalf("read-only argv=%q want=%q", args, want)
	}
	args, err = jsonArgumentsForProfileWithSessionDir(readOnly, true, true, "/home/pi/sessions/project")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"--mode", "json", "--tools", readOnlyTools, "--no-extensions", "--session-dir", "/home/pi/sessions/project"}
	if !slices.Equal(args, want) {
		t.Fatalf("pinned read-only argv=%q want=%q", args, want)
	}

	write := planRequest(config.ModeWorkspaceWrite)
	write.RequestedConfig.Model = "openai/gpt-5"
	write.RequestedConfig.Effort = "high"
	write.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testUUID, PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	args, err = printArguments(write)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"--mode", "json", "--tools", workspaceWriteTools, "--model", "openai/gpt-5", "--thinking", "high", "--session", testUUID}
	if !slices.Equal(args, want) {
		t.Fatalf("workspace-write argv=%q want=%q", args, want)
	}
	if !slices.Contains(args, "--tools") || slices.Contains(args, readOnlyTools) {
		t.Fatalf("workspace-write argv=%q", args)
	}
	if slices.Contains(args, "--no-extensions") {
		t.Fatalf("workspace-write argv disabled native extensions: %q", args)
	}
	if _, err = legacyJSONArguments(write); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("legacy workspace-write error=%v want unsupported profile", err)
	}
}

func TestLegacyJSONArgumentsRetainHistoricalStartupControls(t *testing.T) {
	args, err := legacyJSONArguments(planRequest(ModeReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--mode", "json", "--tools", readOnlyTools, "--no-extensions", "--offline"}
	if !slices.Equal(args, want) {
		t.Fatalf("historical argv=%q want=%q", args, want)
	}
}

func TestJSONArgumentsRejectUnsupportedRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"provider":            func(r *task.TaskRecord) { r.Provider = config.ProviderCodexExec },
		"permission mismatch": func(r *task.TaskRecord) { r.RequestedConfig.Permission = config.ModeWorkspaceWrite },
		"unknown mode":        func(r *task.TaskRecord) { r.Mode = "prompt"; r.RequestedConfig.Permission = "prompt" },
		"native timeout":      func(r *task.TaskRecord) { r.RequestedConfig.NativeTimeout = "1s" },
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

func TestPrepareCandidatePreservesThinkingLiterals(t *testing.T) {
	cases := []struct{ name, effort string }{
		{"omitted", ""},
		{"default", "default"},
		{"off", "off"},
		{"minimal", "minimal"},
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
		{"future", "turbo"},
		{"case", "HIGH"},
		{"internal space", "future effort"},
		{"punctuation", `future:"level"/v2;$(literal)\value`},
		{"unicode", "思考-é"},
		{"replacement rune", "\ufffd"},
		{"internal unicode space", "future\u00a0effort"},
		{"byte limit", strings.Repeat("a", task.MaxControlRecordSize)},
		{"unicode byte limit", strings.Repeat("é", task.MaxControlRecordSize/2)},
	}
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		for _, testCase := range cases {
			t.Run(mode+"/"+testCase.name, func(t *testing.T) {
				testThinkingLiteralPreparation(t, mode, testCase.effort)
			})
		}
	}
}

func testThinkingLiteralPreparation(t *testing.T, mode, effort string) {
	t.Helper()
	request, now := profileReconstructionFixture(t, mode)
	sessionDir, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(piSessionDirEnv, sessionDir)
	request.RequestedConfig.Model = "fixture-model"
	baseline := finalizeProfile(t, mustPrepareCandidate(t, request), now)
	request.RequestedConfig.Effort = effort
	candidate := mustPrepareCandidate(t, request)
	prepared := finalizeProfile(t, candidate, now)
	want := []string{"--mode", "json", "--tools", "read,bash,edit,write"}
	if mode == ModeReadOnly {
		want = []string{"--mode", "json", "--tools", "read,grep,find,ls", "--no-extensions", "--session-dir", sessionDir}
	}
	want = append(want, "--model", "fixture-model")
	if effort != "" && effort != "default" {
		want = append(want, "--thinking", effort)
	}
	if !slices.Equal(prepared.Plan.Arguments, want) {
		t.Fatal("prepared argv did not preserve the exact thinking literal as one value")
	}
	if !task.CompareEffectiveConfigs(prepared.Effective, baseline.Effective) || !prepared.Plan.Predicate.Equal(baseline.Plan.Predicate) {
		t.Fatal("thinking selection changed effective configuration or predicate")
	}
	if candidate.Admit == nil {
		t.Fatal("current candidate has no static admission")
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(admitted.Plan.Arguments, want) || !task.CompareEffectiveConfigs(admitted.Effective, baseline.Effective) || !admitted.Plan.Predicate.Equal(baseline.Plan.Predicate) {
		t.Fatal("static admission changed thinking argv, effective configuration or predicate")
	}
}

func TestPrepareCandidateRejectsMalformedThinkingLiterals(t *testing.T) {
	cases := []struct{ name, effort string }{
		{"leading space", " high"},
		{"trailing space", "high "},
		{"blank", " "},
		{"padded default", "default "},
		{"leading unicode space", "\u00a0high"},
		{"trailing unicode space", "high\u2003"},
		{"tab", "hi\tgh"},
		{"NUL", "hi\x00gh"},
		{"newline", "hi\ngh"},
		{"carriage return", "hi\rgh"},
		{"C0 start", "hi\x01gh"},
		{"C0 end", "hi\x1fgh"},
		{"C1 start", "hi\u0080gh"},
		{"C1 space", "hi\u0085gh"},
		{"C1 end", "hi\u009fgh"},
		{"DEL", "hi\x7fgh"},
		{"invalid UTF-8", "hi\xffgh"},
		{"overlong UTF-8", "hi\xc0\xafgh"},
		{"over byte limit", strings.Repeat("a", task.MaxControlRecordSize+1)},
		{"over unicode byte limit", strings.Repeat("é", task.MaxControlRecordSize/2) + "a"},
		{"dash", "-"},
		{"option", "--help"},
		{"short option", "-h"},
	}
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			request, _ := profileReconstructionFixture(t, mode)
			for _, testCase := range cases {
				t.Run(testCase.name, func(t *testing.T) {
					request := request
					request.RequestedConfig.Effort = testCase.effort
					if _, err := PrepareCandidate(request, []string{"HOME=" + t.TempDir()}); !errors.Is(err, ErrUnsupportedProfile) || !strings.Contains(err.Error(), "thinking") {
						t.Fatalf("malformed thinking literal error=%v want thinking validation before executable lookup", err)
					}
				})
			}
		})
	}
}

func TestRuntimeRequirementsCoverAllLaunchFlags(t *testing.T) {
	requirements := RuntimeRequirements(ModeReadOnly)
	for _, flag := range []string{"--mode", "--tools", "--model", "--thinking", "--session", "--session-dir", "--no-extensions"} {
		if !slices.Contains(requirements.RequiredFlags, flag) {
			t.Fatalf("required flags=%q missing %q", requirements.RequiredFlags, flag)
		}
	}
	if !slices.Equal(requirements.HelpArgs, []string{"--no-extensions"}) {
		t.Fatalf("native Pi help args=%q", requirements.HelpArgs)
	}
	for _, removed := range []string{"--offline"} {
		if slices.Contains(requirements.RequiredFlags, removed) {
			t.Fatalf("native Pi runtime requirements retain removed flag %q", removed)
		}
	}
}

func TestWorkspaceRuntimeRequirementsMatchWorkspaceLaunch(t *testing.T) {
	requirements := RuntimeRequirements(ModeWorkspaceWrite)
	want := []string{"--mode", "--tools", "--model", "--thinking", "--session"}
	if !slices.Equal(requirements.RequiredFlags, want) {
		t.Fatalf("workspace-write runtime requirements=%q want=%q", requirements.RequiredFlags, want)
	}
	if len(requirements.HelpArgs) != 0 {
		t.Fatalf("workspace-write help args=%q", requirements.HelpArgs)
	}
}

func TestWorkspaceWriteContractBindsExactToolSet(t *testing.T) {
	contract := Contract(ModeWorkspaceWrite)
	if !strings.Contains(contract, `"tools":"`+workspaceWriteTools+`"`) {
		t.Fatalf("workspace-write contract does not document built-in tools %q: %s", workspaceWriteTools, contract)
	}
	request := planRequest(ModeWorkspaceWrite)
	arguments, err := jsonArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(arguments, []string{"--mode", "json", "--tools", workspaceWriteTools}) {
		t.Fatalf("workspace-write plan=%q", arguments)
	}
	requirements := RuntimeRequirements(ModeWorkspaceWrite)
	if !slices.Contains(requirements.RequiredFlags, "--tools") {
		t.Fatalf("workspace-write runtime capability omits --tools: %+v", requirements)
	}
}

func TestHistoricalNativeRuntimeRequirementsPreserveSnapshot(t *testing.T) {
	requirements := historicalNativeRuntimeRequirements()
	want := []string{"--mode", "--tools", "--model", "--thinking", "--session"}
	if len(requirements.HelpArgs) != 0 || !slices.Equal(requirements.RequiredFlags, want) {
		t.Fatalf("historical native runtime requirements=%+v", requirements)
	}
}

func TestWorkspaceWriteV7RuntimeRequirementsPreserveSnapshot(t *testing.T) {
	requirements := workspaceWriteV7RuntimeRequirements()
	want := []string{"--mode", "--model", "--thinking", "--session"}
	if len(requirements.HelpArgs) != 0 || !slices.Equal(requirements.RequiredFlags, want) {
		t.Fatalf("workspace-write v7 runtime requirements=%+v", requirements)
	}
}

func TestLegacyRuntimeRequirementsPreserveSnapshotFlagOrder(t *testing.T) {
	requirements := legacyRuntimeRequirements()
	want := []string{
		"--mode", "--tools", "--model", "--thinking", "--session", "--session-dir", "--no-extensions", "--offline",
	}
	if !slices.Equal(requirements.HelpArgs, []string{"--no-extensions", "--offline"}) || !slices.Equal(requirements.RequiredFlags, want) {
		t.Fatalf("legacy runtime requirements=%+v", requirements)
	}
}
