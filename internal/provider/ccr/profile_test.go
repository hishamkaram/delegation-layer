package ccr

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
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func routeFacts(t *testing.T, candidate commonprovider.ProfileCandidate, resumeRoute bool) json.RawMessage {
	t.Helper()
	if candidate.Inspection == nil {
		t.Fatal("candidate has no inspection")
	}
	native, err := task.MarshalCanonical(routeCapabilities{ResumeRoute: resumeRoute})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable,
		SHA256:     candidate.Inspection.ExecutableSHA256,
		Version:    "fixture",
	}, native)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func legacyFacts(t *testing.T, candidate commonprovider.ProfileCandidate) json.RawMessage {
	t.Helper()
	if candidate.Inspection == nil {
		t.Fatal("candidate has no inspection")
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable,
		SHA256:     candidate.Inspection.ExecutableSHA256,
		Version:    "fixture",
	}, json.RawMessage(`{"forwarded_flags_verified":true}`))
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func testRequest() task.TaskRecord {
	return task.TaskRecord{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), Provider: Provider, Mode: "read-only", CanonicalCwd: "/workspace", BriefLength: 20, BudgetNanos: int64(time.Minute), RequestedConfig: task.TaskConfig{Model: "local-model", Permission: "read-only"}}
}

func TestForegroundArgumentsAndExactContinuation(t *testing.T) {
	request := testRequest()
	request.RequestedConfig.Effort = "high"
	args, permission, err := arguments(request)
	if err != nil {
		t.Fatal(err)
	}
	session, err := claude.FreshSessionID(request.RootID, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"launch", "--model", "local-model", "--auth-mode", "provider-only", "--print", "--permission-mode", "plan", "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--effort", "high", "--session-id", session}
	if !slices.Equal(args, expected) || permission != "plan" {
		t.Fatalf("args=%v permission=%s", args, permission)
	}
	request.Mode, request.RequestedConfig.Permission = "workspace-write", "workspace-write"
	request.RequestedConfig.Effort = "default"
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: session, PredecessorTaskID: strings.Repeat("c", 32)}
	args, permission, err = arguments(request)
	if err != nil {
		t.Fatal(err)
	}
	expected = []string{"launch", "--resume-route", "--print", "--permission-mode", "bypassPermissions", "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--resume", session}
	if permission != "bypassPermissions" || !slices.Equal(args, expected) || slices.Contains(args, "--model") || slices.Contains(args, "--auth-mode") || slices.Contains(args, "--session-id") {
		t.Fatalf("resume argv=%v", args)
	}
	request.PriorSession.Provider = claude.Provider
	if _, _, err := arguments(request); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("cross-adapter continuation: %v", err)
	}
}

func TestContinuationModelChangeFollowsRecordedRoute(t *testing.T) {
	predecessor := testRequest()
	successor := predecessor
	successor.RequestedConfig.Model = "other-alias"
	for name, test := range map[string]struct {
		reference task.PredicateRef
		wantErr   error
	}{
		"route-preserving": {reference: Reference(predecessor.Mode), wantErr: commonprovider.ErrUnsupportedOption},
		"modeled":          {reference: modeledReference(predecessor.Mode)},
		"previous":         {reference: previousReference(predecessor.Mode)},
		"historical":       {reference: HistoricalReference(predecessor.Mode)},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateContinuation(predecessor, task.MetaRecord{Predicate: test.reference}, successor)
			if test.wantErr == nil && err != nil {
				t.Fatalf("changed model continuation error = %v, want nil", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("changed model continuation error = %v, want %v", err, test.wantErr)
			}
		})
	}
	if err := validateContinuation(predecessor, task.MetaRecord{Predicate: Reference(predecessor.Mode)}, predecessor); err != nil {
		t.Fatalf("unchanged model continuation error = %v", err)
	}
}

func TestLegacyRuntimeProjectionChecksForwardedEffort(t *testing.T) {
	request := testRequest()
	help := []byte("--input-format --output-format --verbose --permission-prompts --permission-mode --session-id --resume")
	if _, err := helpProjector(request)(help); err != nil {
		t.Fatal(err)
	}
	request.RequestedConfig.Effort = "high"
	if _, err := helpProjector(request)(help); err == nil {
		t.Fatal("missing forwarded effort accepted")
	}
	if _, err := helpProjector(request)(append(help, []byte(" --effort")...)); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRequiresAliasAndCopiesMetadata(t *testing.T) {
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.RequestedConfig.Model = ""
	if err := catalog.ValidateRequest(request); !errors.Is(err, commonprovider.ErrUnsupportedOption) || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("missing alias: %v", err)
	}
	description := catalog.Descriptions()[0]
	description.RequiredOptions[0] = "mutated"
	if catalog.Descriptions()[0].RequiredOptions[0] != commonprovider.OptionModel {
		t.Fatal("required options escaped catalog ownership")
	}
	request.RequestedConfig.Model = "not-yet-discovered"
	if err := catalog.ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeRequiresRuntimeFacts(t *testing.T) {
	request := testRequest()
	cli := commonprovider.CLIInfo{Path: "/usr/bin/ccr", SHA256: strings.Repeat("d", 64)}
	args, permission, err := arguments(request)
	if err != nil {
		t.Fatal(err)
	}
	facts, factsErr := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: cli.Path, SHA256: cli.SHA256, Version: "ccr changed-build"}, nil)
	if factsErr != nil {
		t.Fatal(factsErr)
	}
	profile, finalErr := finalize(request, args, permission, cli, []string{"HOME=/home/test"}, []string{"/tmp"}, Reference(request.Mode), facts)
	if finalErr != nil {
		t.Fatal(finalErr)
	}
	if profile.Plan.Predicate.Adapter != Provider || profile.Effective.Approval != "plan" || len(profile.Plan.InputFiles) != 0 {
		t.Fatalf("profile=%+v", profile)
	}
}

func TestFreshProfileSelectsRouteCompatiblePreparation(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "ccr"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)

	request := testRequest()
	request.CanonicalCwd = directory
	for name, resumeRoute := range map[string]bool{"legacy": false, "route-preserving": true} {
		t.Run(name, func(t *testing.T) {
			candidate, err := PrepareCandidate(request)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Inspection == nil || !slices.Equal(candidate.Inspection.Arguments, []string{"launch", "--help"}) || slices.Contains(candidate.Inspection.Runtime.RequiredFlags, "--resume-route") {
				t.Fatalf("inspection=%+v", candidate.Inspection)
			}
			profile, err := candidate.Finalize(routeFacts(t, candidate, resumeRoute), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			want := modeledReference(request.Mode)
			if resumeRoute {
				want = Reference(request.Mode)
			}
			if !profile.Plan.Predicate.Equal(want) {
				t.Fatalf("predicate=%+v want=%+v", profile.Plan.Predicate, want)
			}
		})
	}
}

func TestEnvironmentDeclaresCCRRootWithoutCopyingSecrets(t *testing.T) {
	env, roots, err := nativeEnvironment([]string{"HOME=/home/test", "PATH=/usr/bin", "XDG_DATA_HOME=/data", "API_KEY=private-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(roots, "/data/claude-code-router") || strings.Contains(strings.Join(env, "\n"), "private-secret") {
		t.Fatalf("env=%v roots=%v", env, roots)
	}
}
