package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestFinalizeNativePreparedProfileUsesRuntimeFacts(t *testing.T) {
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
	prepared, err := finalizeNativePreparedProfile(request, arguments, inputs, cli, environment, strings.Repeat("b", 64), json.RawMessage(facts), time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ObservedVersion != "Claude Code 99.7.3" || prepared.Effective.Approval != "plan" || prepared.Effective.Policy == nil || prepared.Effective.Policy.ProfileRevision != commonprovider.NativeProfileRevision || len(prepared.Effective.Policy.Sources) != 0 || len(prepared.Plan.InputFiles) != 0 {
		t.Fatalf("native runtime facts were not finalized: %+v", prepared)
	}
}

func TestFinalizeHistoricalNativePreparedProfilePreservesRecordedContract(t *testing.T) {
	request := profileRequest()
	arguments, inputs, err := historicalNativePrintArguments(request)
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
		Version:    "Claude Code 2.1.270",
		SHA256:     cli.SHA256,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := finalizeNativePreparedProfileWithContract(request, arguments, inputs, cli, environment, strings.Repeat("b", 64), json.RawMessage(facts), time.Unix(2_000_000_000, 0), nativePreparationOptions{
		permissionMode:     "dontAsk",
		predicateReference: legacyNativeReferenceForMode(Mode),
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Effective.Approval != "dontAsk" || !prepared.Plan.Predicate.Equal(legacyNativeReferenceForMode(Mode)) {
		t.Fatalf("historical native contract changed: %+v", prepared)
	}
}

func TestExistingNativeReadOnlyReconstructsRecordedLaunchContract(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	claude := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claude, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	cli, err := commonprovider.LocateCLIPath(claude)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	request := profileRequest()
	request.CanonicalCwd = filepath.Join(home, "workspace")
	meta := task.MetaRecord{
		Environment: []string{"HOME=" + home, "PATH=" + binDir, "XDG_CONFIG_HOME=relative"},
		Predicate:   legacyNativeReferenceForMode(Mode),
		EffectiveConfig: task.EffectiveConfig{
			Approval: "dontAsk",
			Policy: &task.PolicyDetails{
				ProfileRevision: commonprovider.NativeProfileRevision,
				WritableRoots:   []string{"/tmp", filepath.Join(home, ".claude")},
			},
		},
	}
	candidate, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(candidate.Inspection.Environment, "XDG_CONFIG_HOME=relative") {
		t.Fatalf("historical environment was not retained: %q", candidate.Inspection.Environment)
	}
	if candidate.Inspection.Runtime == nil || !slices.Contains(candidate.Inspection.Runtime.RequiredFlags, "--permission-mode") {
		t.Fatalf("runtime inspection omitted permission flag: %+v", candidate.Inspection)
	}
	if !slices.Contains(candidate.WritableRoots, "/tmp") || !slices.Equal(candidate.WritableRoots, meta.EffectiveConfig.Policy.WritableRoots) {
		t.Fatalf("recorded roots changed: got=%q want=%q", candidate.WritableRoots, meta.EffectiveConfig.Policy.WritableRoots)
	}
	if !slices.Contains(candidate.Inspection.Environment, "HOME="+home) {
		t.Fatalf("recorded HOME was not used: %q", candidate.Inspection.Environment)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: cli.Path, SHA256: cli.SHA256, Version: "Claude Code 2.1.270"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Effective.Approval != "dontAsk" || !profile.Plan.Predicate.Equal(legacyNativeReferenceForMode(Mode)) {
		t.Fatalf("recorded native contract changed: %+v", profile)
	}
}

func TestFinalizeNativeWorkspaceWriteUsesBypassPermissions(t *testing.T) {
	request := profileRequest()
	request.Mode = WorkspaceWriteMode
	request.RequestedConfig.Permission = WorkspaceWriteMode
	arguments, inputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(arguments, "bypassPermissions") || slices.Contains(arguments, "acceptEdits") {
		t.Fatalf("workspace-write native argv=%q", arguments)
	}
	cli := commonprovider.CLIInfo{Path: "/usr/local/bin/claude", SHA256: strings.Repeat("a", 64)}
	environment := profileEnvironment{RuntimeSHA256: cli.SHA256, WritableRoots: []string{"/home/test/.claude"}}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: cli.Path, Version: "Claude Code 99.7.3", SHA256: cli.SHA256,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := finalizeNativePreparedProfile(request, arguments, inputs, cli, environment, strings.Repeat("b", 64), json.RawMessage(facts), time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Effective.Approval != "bypassPermissions" || !prepared.Plan.Predicate.Equal(WorkspaceWriteReference()) {
		t.Fatalf("workspace-write native contract=%+v", prepared)
	}
}

func TestExistingNativeWorkspaceWriteReconstructsAcceptEditsContract(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	claude := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claude, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	cli, err := commonprovider.LocateCLIPath(claude)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	request := profileRequest()
	request.Mode = WorkspaceWriteMode
	request.CanonicalCwd = filepath.Join(home, "workspace")
	request.RequestedConfig.Permission = WorkspaceWriteMode
	wantPredicate := legacyNativeReferenceForMode(WorkspaceWriteMode)
	meta := task.MetaRecord{
		Environment: []string{"HOME=" + home, "PATH=" + binDir, "XDG_CONFIG_HOME=relative"},
		Approval:    "acceptEdits",
		Predicate:   wantPredicate,
		EffectiveConfig: task.EffectiveConfig{
			Approval: "acceptEdits",
			Policy: &task.PolicyDetails{
				ProfileRevision: commonprovider.NativeProfileRevision,
				WritableRoots:   []string{"/tmp", filepath.Join(home, ".claude")},
			},
		},
	}
	candidate, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(candidate.Inspection.Environment, "XDG_CONFIG_HOME=relative") {
		t.Fatalf("historical environment was not retained: %q", candidate.Inspection.Environment)
	}
	if !slices.Equal(candidate.WritableRoots, meta.EffectiveConfig.Policy.WritableRoots) {
		t.Fatalf("recorded roots changed: got=%q want=%q", candidate.WritableRoots, meta.EffectiveConfig.Policy.WritableRoots)
	}
	if !slices.Contains(candidate.Inspection.Environment, "HOME="+home) {
		t.Fatalf("recorded HOME was not used: %q", candidate.Inspection.Environment)
	}
	args := candidate.Inspection.Runtime
	if args == nil || !slices.Contains(args.RequiredFlags, "--permission-mode") {
		t.Fatalf("runtime inspection omitted permission flag: %+v", candidate.Inspection)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: cli.Path, SHA256: cli.SHA256, Version: "Claude Code 2.1.270"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Effective.Approval != "acceptEdits" || !profile.Plan.Predicate.Equal(wantPredicate) {
		t.Fatalf("recorded workspace-write contract changed: %+v", profile)
	}
	if !slices.Contains(profile.Plan.Arguments, "acceptEdits") || slices.Contains(profile.Plan.Arguments, "bypassPermissions") {
		t.Fatalf("historical workspace-write argv changed: %q", profile.Plan.Arguments)
	}
}

func TestLegacyNativePolicyMarkerIsRecognizedForExistingTasks(t *testing.T) {
	without := task.MetaRecord{EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{Sources: []task.PolicySourceDigest{{Kind: "claude-user-settings", Present: true}}}}}
	if hasLegacyNativePolicy(without) {
		t.Fatal("ordinary portable policy was classified as legacy")
	}
	with := task.MetaRecord{EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{Sources: []task.PolicySourceDigest{{Kind: "native-oauth-policy-proof", Present: true}}}}}
	if !hasLegacyNativePolicy(with) {
		t.Fatal("legacy native policy marker was not recognized")
	}
}

func TestFinalizeLegacyPreparedProfilePreservesStoredPredicate(t *testing.T) {
	request := profileRequest()
	arguments, inputs, err := legacyPrintArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	cli := commonprovider.CLIInfo{Path: "/usr/local/bin/claude", SHA256: strings.Repeat("a", 64)}
	environment := profileEnvironment{RuntimeSHA256: cli.SHA256}
	native, err := projectNativePolicy(nativePolicyFixture(t, "team", time.Unix(2_000_000_000, 0).Add(time.Hour).UnixMilli()), 404, nil)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: cli.Path, Version: "Claude Code 2.1.270", SHA256: cli.SHA256}, native)
	if err != nil {
		t.Fatal(err)
	}
	wantPredicate := LegacyReference()
	prepared, err := finalizeLegacyPreparedProfile(request, arguments, inputs, cli, environment, nil, strings.Repeat("b", 64), wantPredicate, facts, time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Plan.Predicate.Equal(wantPredicate) || prepared.Effective.Policy == nil {
		t.Fatalf("legacy profile did not preserve stored predicate and policy: %+v", prepared)
	}
}

func TestPrintArgumentsPreserveContainmentAndTaskOwnedFilesOnResume(t *testing.T) {
	request := profileRequest()
	args, inputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--print", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-mode", "plan", "--permission-prompts", "none"}
	fresh, err := FreshSessionID(request.RootID, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(args, append(slices.Clone(want), "--session-id", fresh)) {
		t.Fatalf("fresh argv=%q", args)
	}
	if len(inputs) != 0 {
		t.Fatalf("native configuration declarations=%+v", inputs)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: fresh, PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	resumed, resumeInputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resumed, append(slices.Clone(want), "--resume", fresh)) || len(resumeInputs) != 0 {
		t.Fatalf("resume changed native argv or input declarations: %q %+v", resumed, resumeInputs)
	}
}

func TestPrintArgumentsPassModelAndEffortAndOmitDefault(t *testing.T) {
	request := profileRequest()
	request.RequestedConfig.Model = "claude-sonnet-4"
	request.RequestedConfig.Effort = "max"
	args, inputs, err := printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := FreshSessionID(request.RootID, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--print", "--input-format", "text", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "plan", "--permission-prompts", "none",
		"--model", "claude-sonnet-4", "--effort", "max", "--session-id", fresh,
	}
	if !slices.Equal(args, want) || len(inputs) != 0 {
		t.Fatalf("argv=%q inputs=%+v", args, inputs)
	}
	request.RequestedConfig.Effort = "default"
	args, _, err = printArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "--effort") || slices.Contains(args, "--model") == false {
		t.Fatalf("default effort was not omitted: %q", args)
	}
}

func TestRuntimeRequirementsAddRequestedModelAndEffort(t *testing.T) {
	base := runtimeRequirementsForRequest(profileRequest())
	if slices.Contains(base.RequiredFlags, "--model") || slices.Contains(base.RequiredFlags, "--effort") {
		t.Fatalf("default runtime requirements contain optional flags: %q", base.RequiredFlags)
	}
	request := profileRequest()
	request.RequestedConfig.Model = "claude-sonnet-4"
	request.RequestedConfig.Effort = "max"
	got := runtimeRequirementsForRequest(request)
	if !slices.Contains(got.RequiredFlags, "--model") || !slices.Contains(got.RequiredFlags, "--effort") {
		t.Fatalf("requested runtime requirements omitted optional flags: %q", got.RequiredFlags)
	}
}

func TestLegacyPrintArgumentsPreserveHistoricalRestrictionsAndInputs(t *testing.T) {
	request := profileRequest()
	args, inputs, err := legacyPrintArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--safe-mode", "--restricted", "--tools", "--disallowedTools", "--strict-mcp-config", "--mcp-config", "--settings", "--disable-slash-commands", "--no-chrome"} {
		if !slices.Contains(args, flag) {
			t.Fatalf("legacy argv omitted %s: %q", flag, args)
		}
	}
	if len(inputs) != 2 || inputs[0].Name != "claude-profile.json" || inputs[1].Name != "empty-mcp.json" {
		t.Fatalf("legacy input declarations=%+v", inputs)
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
	if !slices.Contains(args, "bypassPermissions") || slices.Contains(args, "acceptEdits") || slices.Contains(args, "--safe-mode") || slices.Contains(args, "--restricted") {
		t.Fatalf("workspace-write argv=%q", args)
	}
	if len(inputs) != 0 {
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
		"model newline":     func(r *task.TaskRecord) { r.RequestedConfig.Model = "other\nmodel" },
		"effort tab":        func(r *task.TaskRecord) { r.RequestedConfig.Effort = "max\t" },
		"oversized model":   func(r *task.TaskRecord) { r.RequestedConfig.Model = strings.Repeat("m", task.MaxControlRecordSize+1) },
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
