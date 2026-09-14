package contributorprovider

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

func TestDescriptionUsesMeasuredCertificationWithoutLiveProbes(t *testing.T) {
	record, err := embeddedCertification()
	if err != nil {
		t.Fatal(err)
	}
	description := Description()
	if description.ID != Provider || !description.Discoverable || !slices.Equal(description.SupportedOptions, []string{commonprovider.OptionContinuation}) {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Profiles) != 1 || description.Profiles[0] != record.profile() {
		t.Fatalf("description drifted from embedded certification: %+v", description.Profiles)
	}
	if _, err = commonprovider.NewCatalog(Registration()); err != nil {
		t.Fatalf("registration was rejected: %v", err)
	}
}

func TestPrepareBuildsExactPlanAndPolicy(t *testing.T) {
	workspace, runtimeDir := separateDirectories(t)
	request := testRequest(workspace)
	profile := prepareTestProfile(t, request, runtimeDir)
	assertPreparedPlan(t, profile, workspace)
	assertPreparedInputs(t, profile, runtimeDir)
	assertPreparedPolicy(t, profile, workspace, runtimeDir)
	if err := profile.Validate(request); err != nil {
		t.Fatalf("prepared profile failed validation: %v", err)
	}
}

func assertPreparedPlan(t *testing.T, profile commonprovider.PreparedProfile, workspace string) {
	t.Helper()
	wantArguments := []string{"--runtime-config", "", "--tools-config", "", "--output", "", "--task-id", testTaskID, "--session-id", "session-" + testTaskID}
	if !slices.Equal(profile.Plan.Arguments, wantArguments) {
		t.Fatalf("argv = %#v, want %#v", profile.Plan.Arguments, wantArguments)
	}
	if profile.Plan.Executable != "/test/bin/provider" || profile.Plan.Directory != workspace || profile.Plan.Environment == nil || len(profile.Plan.Environment) != 0 {
		t.Fatalf("unexpected plan identity: %+v", profile.Plan)
	}
	if !profile.Plan.Predicate.Equal(Reference()) || profile.Plan.OutputWriterContract != OutputWriterContract {
		t.Fatalf("unexpected plan contract: %+v", profile.Plan)
	}
	if len(profile.Plan.OutputArtifacts) != 1 || profile.Plan.OutputArtifacts[0] != (task.OutputArtifact{Name: OutputName, ArgumentIndex: 5}) {
		t.Fatalf("unexpected output declarations: %+v", profile.Plan.OutputArtifacts)
	}
}

func assertPreparedInputs(t *testing.T, profile commonprovider.PreparedProfile, runtimeDir string) {
	t.Helper()
	if len(profile.Plan.InputFiles) != 2 || profile.Plan.InputFiles[0].Name != RuntimeInputName || profile.Plan.InputFiles[0].ArgumentIndex != 1 || profile.Plan.InputFiles[1].Name != ToolsInputName || profile.Plan.InputFiles[1].ArgumentIndex != 3 {
		t.Fatalf("unexpected input declarations: %+v", profile.Plan.InputFiles)
	}
	var runtimeConfig protocol.RuntimeConfig
	if err := task.DecodeStrict([]byte(profile.Plan.InputFiles[0].Content), &runtimeConfig); err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.SchemaVersion != protocol.SchemaVersion || runtimeConfig.RuntimeDir != runtimeDir {
		t.Fatalf("runtime input = %+v", runtimeConfig)
	}
	var toolsConfig protocol.ToolsConfig
	if err := task.DecodeStrict([]byte(profile.Plan.InputFiles[1].Content), &toolsConfig); err != nil {
		t.Fatal(err)
	}
	if toolsConfig.SchemaVersion != protocol.SchemaVersion || toolsConfig.WorkspaceWrite || len(toolsConfig.AllowedTools) != 0 {
		t.Fatalf("tools input = %+v", toolsConfig)
	}
}

func assertPreparedPolicy(t *testing.T, profile commonprovider.PreparedProfile, workspace, runtimeDir string) {
	t.Helper()
	if profile.Effective.Policy == nil || profile.Effective.Policy.ProfileRevision == "" || profile.Effective.Policy.RuntimeSHA256 == "" || profile.Effective.Policy.Workspace != workspace || !slices.Equal(profile.Effective.Policy.WritableRoots, []string{runtimeDir}) {
		t.Fatalf("unexpected effective policy: %+v", profile.Effective)
	}
	withoutDigest := profile.Effective
	withoutDigest.Digest = ""
	encoded, err := task.MarshalCanonical(withoutDigest)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Effective.Digest != task.ComputeSHA256(encoded) {
		t.Fatal("effective digest was not computed from canonical empty-digest policy")
	}
}

func TestPrepareContinuationUsesExactPriorSession(t *testing.T) {
	workspace, runtimeDir := separateDirectories(t)
	request := testRequest(workspace)
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "session-cccccccccccccccccccccccccccccccc", PredecessorTaskID: "dddddddddddddddddddddddddddddddd"}
	profile := prepareTestProfile(t, request, runtimeDir)
	want := []string{"--runtime-config", "", "--tools-config", "", "--output", "", "--task-id", testTaskID, "--session-id", request.PriorSession.ConversationID, "--resume"}
	if !slices.Equal(profile.Plan.Arguments, want) {
		t.Fatalf("continuation argv = %#v, want %#v", profile.Plan.Arguments, want)
	}
}

func TestPrepareRejectsWorkspaceRuntimeOverlapAndIdentityDrift(t *testing.T) {
	workspace, runtimeDir := separateDirectories(t)
	request := testRequest(workspace)
	record, err := embeddedCertification()
	if err != nil {
		t.Fatal(err)
	}
	identity := runtimeIdentity{Executable: "/test/bin/provider", Version: record.ProviderVersion, SHA256: record.RuntimeSHA256, OS: record.OS, Arch: record.Arch}
	cases := []struct {
		name string
		deps prepareDependencies
	}{
		{name: "workspace runtime overlap", deps: prepareDependencies{Identity: identity, RuntimeDir: workspace, Certification: record}},
		{name: "binary digest drift", deps: prepareDependencies{Identity: runtimeIdentity{Executable: identity.Executable, Version: identity.Version, SHA256: strings.Repeat("f", 64), OS: identity.OS, Arch: identity.Arch}, RuntimeDir: runtimeDir, Certification: record}},
		{name: "platform drift", deps: prepareDependencies{Identity: runtimeIdentity{Executable: identity.Executable, Version: identity.Version, SHA256: identity.SHA256, OS: "linux", Arch: identity.Arch}, RuntimeDir: runtimeDir, Certification: record}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prepareWithDependencies(request, tc.deps); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("invalid preparation was accepted: %v", err)
			}
		})
	}
}

func TestPrepareRejectsNonCanonicalOrMissingRuntime(t *testing.T) {
	workspace, runtimeDir := separateDirectories(t)
	request := testRequest(workspace)
	record, err := embeddedCertification()
	if err != nil {
		t.Fatal(err)
	}
	identity := runtimeIdentity{Executable: "/test/bin/provider", Version: record.ProviderVersion, SHA256: record.RuntimeSHA256, OS: record.OS, Arch: record.Arch}
	for _, runtimePath := range []string{filepath.Join(runtimeDir, "missing"), runtimeDir + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(runtimeDir)} {
		if _, err := prepareWithDependencies(request, prepareDependencies{Identity: identity, RuntimeDir: runtimePath, Certification: record}); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("runtime path %q was accepted: %v", runtimePath, err)
		}
	}
	if _, err := prepareWithDependencies(request, prepareDependencies{Identity: identity, RuntimeDir: "relative", Certification: record}); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("relative runtime path was accepted: %v", err)
	}
}

func TestResolveRuntimeDirectoryRequiresExplicitCanonicalDirectory(t *testing.T) {
	t.Setenv(contributorRuntimeEnvironment, "")
	if _, err := resolveRuntimeDirectory(); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("missing runtime was accepted: %v", err)
	}
	t.Setenv(contributorRuntimeEnvironment, filepath.Join(t.TempDir(), "missing"))
	if _, err := resolveRuntimeDirectory(); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("missing runtime directory was accepted: %v", err)
	}
}

func separateDirectories(t *testing.T) (string, string) {
	t.Helper()
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	runtimeDir := filepath.Join(parent, "runtime")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalWorkspace, err := config.CanonicalizePath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRuntime, err := config.CanonicalizePath(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	return canonicalWorkspace, canonicalRuntime
}

func testRequest(workspace string) task.TaskRecord {
	return task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: testRootID, TaskID: testTaskID, Provider: Provider, Mode: Mode, CanonicalCwd: workspace, RequestedConfig: task.TaskConfig{Permission: Mode}, BudgetNanos: int64(time.Minute), BriefSHA256: strings.Repeat("a", 64), BriefLength: 1}
}

func prepareTestProfile(t *testing.T, request task.TaskRecord, runtimeDir string) commonprovider.PreparedProfile {
	t.Helper()
	record, err := embeddedCertification()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := prepareWithDependencies(request, prepareDependencies{Identity: runtimeIdentity{Executable: "/test/bin/provider", Version: record.ProviderVersion, SHA256: record.RuntimeSHA256, OS: record.OS, Arch: record.Arch}, RuntimeDir: runtimeDir, Certification: record})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
