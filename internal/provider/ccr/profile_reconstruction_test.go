package ccr

import (
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

func TestExistingProfilePreservesPreUpgradePredicate(t *testing.T) {
	for _, mode := range []string{"read-only", "workspace-write"} {
		for _, reference := range []task.PredicateRef{previousReference(mode), HistoricalReference(mode)} {
			t.Run(mode+"/"+reference.Version, func(t *testing.T) {
				testPreUpgradeProfile(t, mode, reference)
			})
		}
	}
}

func testPreUpgradeProfile(t *testing.T, mode string, reference task.PredicateRef) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "ccr"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := testRequest()
	request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := candidate.Finalize(routeFacts(t, candidate, true), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	existing, err := PrepareExistingCandidate(request, task.MetaRecord{Predicate: reference, EffectiveConfig: current.Effective, Environment: current.Plan.Environment, EnvironmentRecorded: true})
	if err != nil {
		t.Fatal(err)
	}
	if existing.Inspection == nil || !slices.Equal(existing.Inspection.Arguments, []string{"launch", "--", "--help"}) {
		t.Fatalf("legacy inspection changed: %+v", existing.Inspection)
	}
	if _, err = existing.Finalize(routeFacts(t, existing, false), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("legacy predicate accepted route proof: %v", err)
	}
	rebuilt, err := existing.Finalize(legacyFacts(t, existing), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !current.Plan.Predicate.Equal(Reference(mode)) || !rebuilt.Plan.Predicate.Equal(reference) || !slices.Equal(current.Plan.Arguments, rebuilt.Plan.Arguments) || !task.CompareEffectiveConfigs(current.Effective, rebuilt.Effective) {
		t.Fatalf("predicate or launch contract changed: current=%+v rebuilt=%+v", current.Plan.Predicate, rebuilt.Plan.Predicate)
	}
}

func TestExistingContinuationPreservesPreResumeRouteArguments(t *testing.T) {
	for _, mode := range []string{"read-only", "workspace-write"} {
		for _, reference := range []task.PredicateRef{previousReference(mode), HistoricalReference(mode)} {
			t.Run(mode+"/"+reference.Version, func(t *testing.T) {
				testExistingContinuationPreservesPreResumeRouteArguments(t, mode, reference)
			})
		}
	}
}

func testExistingContinuationPreservesPreResumeRouteArguments(t *testing.T, mode string, reference task.PredicateRef) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "ccr"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := testRequest()
	request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
	request.RequestedConfig.Model = "legacy-override"
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "recorded-session", PredecessorTaskID: strings.Repeat("c", 32)}
	candidate, err := PrepareExistingCandidate(request, task.MetaRecord{Predicate: reference, Environment: []string{"HOME=" + directory, "PATH=" + directory}, EnvironmentRecorded: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(candidate.Inspection.Runtime.RequiredFlags, "--resume-route") {
		t.Fatalf("historical runtime requirements unexpectedly require --resume-route: %q", candidate.Inspection.Runtime.RequiredFlags)
	}
	profile, err := candidate.Finalize(legacyFacts(t, candidate), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	permission := "plan"
	if mode == "workspace-write" {
		permission = "bypassPermissions"
	}
	want := []string{"launch", "--model", "legacy-override", "--auth-mode", "provider-only", "--print", "--permission-mode", permission, "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--resume", "recorded-session"}
	if !profile.Plan.Predicate.Equal(reference) || !slices.Equal(profile.Plan.Arguments, want) || slices.Contains(profile.Plan.Arguments, "--resume-route") {
		t.Fatalf("historical continuation changed: predicate=%+v argv=%q", profile.Plan.Predicate, profile.Plan.Arguments)
	}
}

func TestExistingRoutePreservingProfileRequiresRouteCapability(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "ccr"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := testRequest()
	request.CanonicalCwd = directory
	candidate, err := PrepareExistingCandidate(request, task.MetaRecord{
		Predicate:           Reference(request.Mode),
		Environment:         []string{"HOME=" + directory, "PATH=" + directory},
		EnvironmentRecorded: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = candidate.Finalize(routeFacts(t, candidate, false), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("error=%v want unavailable profile", err)
	}
	if _, err = candidate.Finalize(legacyFacts(t, candidate), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("route-preserving predicate accepted legacy proof: %v", err)
	}
}

func TestExistingModeledProfileReplaysRouteCapabilityProof(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "ccr"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := testRequest()
	request.CanonicalCwd = directory
	fresh, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := fresh.Finalize(routeFacts(t, fresh, false), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.Plan.Predicate.Equal(modeledReference(request.Mode)) {
		t.Fatalf("predicate=%+v want=%+v", admitted.Plan.Predicate, modeledReference(request.Mode))
	}
	existing, err := PrepareExistingCandidate(request, task.MetaRecord{
		Predicate:           admitted.Plan.Predicate,
		EffectiveConfig:     admitted.Effective,
		Environment:         admitted.Plan.Environment,
		EnvironmentRecorded: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if existing.Inspection == nil || existing.Inspection.Revision != routeInspectionRevision || !slices.Equal(existing.Inspection.Arguments, []string{"launch", "--help"}) {
		t.Fatalf("inspection=%+v", existing.Inspection)
	}
	rebuilt, err := existing.Finalize(routeFacts(t, existing, false), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt.Plan.Predicate.Equal(admitted.Plan.Predicate) || !slices.Equal(rebuilt.Plan.Arguments, admitted.Plan.Arguments) || !task.CompareEffectiveConfigs(rebuilt.Effective, admitted.Effective) {
		t.Fatalf("reconstruction changed admitted profile: admitted=%+v rebuilt=%+v", admitted.Plan.Predicate, rebuilt.Plan.Predicate)
	}
	assertModeledProfileRejectsIncompatibleProofs(t, existing)
}

func assertModeledProfileRejectsIncompatibleProofs(t *testing.T, existing commonprovider.ProfileCandidate) {
	t.Helper()
	if _, err := existing.Finalize(routeFacts(t, existing, true), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("modeled predicate accepted route-preserving proof: %v", err)
	}
	if _, err := existing.Finalize(legacyFacts(t, existing), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("modeled predicate accepted legacy proof: %v", err)
	}
}
