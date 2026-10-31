package admission

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TestFactsForCurrentRuntimeKeepsHistoricalVersionOutOfCurrentObservation
// projects saved admission facts onto the current installed executable. The
// saved proof stays byte-identical, and its old version is not reported as an
// observation of the executable that now launches.
func TestFactsForCurrentRuntimeKeepsHistoricalVersionOutOfCurrentObservation(t *testing.T) {
	stored, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: "/opt/provider/old/provider", Version: "provider-1", SHA256: strings.Repeat("a", 64),
	}, json.RawMessage(`{"eligible":true}`))
	if err != nil {
		t.Fatal(err)
	}
	original := string(stored)
	definition := commonprovider.InspectionDefinition{Runtime: &commonprovider.RuntimeProbeDefinition{Executable: "/opt/provider/next/provider"}}
	projected, err := factsForCurrentRuntime(commonprovider.ProfileCandidate{Inspection: &definition}, stored)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != original {
		t.Fatal("saved admission proof was rewritten")
	}
	facts, err := commonprovider.DecodeInspectionFacts(projected)
	if err != nil {
		t.Fatal(err)
	}
	if *facts.Runtime != (commonprovider.RuntimeFacts{Executable: "/opt/provider/next/provider"}) || string(facts.Native) != `{"eligible":true}` {
		t.Fatalf("projected facts = %+v native=%s, want the current path only and the unchanged native projection", facts.Runtime, facts.Native)
	}
}

// TestFinalizeCandidateRecordsPathOnlyProviderRuntime proves a completed
// runtime preflight that observed only the installed path finalizes into a
// nonempty ProviderRuntime with no invented version or digest.
func TestFinalizeCandidateRecordsPathOnlyProviderRuntime(t *testing.T) {
	request := task.TaskRecord{
		Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace",
		RequestedConfig: task.TaskConfig{Permission: "read-only", Budget: "1s"}, BudgetNanos: int64(time.Second),
	}
	definition := commonprovider.InspectionDefinition{Runtime: &commonprovider.RuntimeProbeDefinition{Executable: "/opt/provider/provider"}}
	candidate := commonprovider.ProfileCandidate{
		Directory: request.CanonicalCwd, Inspection: &definition,
		Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
			profile := commonprovider.PreparedProfile{
				Effective: task.EffectiveConfig{Containment: "read-only", Approval: "never", Digest: task.ComputeSHA256([]byte("config"))},
			}
			profile.Plan.Executable, profile.Plan.Arguments, profile.Plan.Directory = "/opt/provider/provider", []string{"-"}, request.CanonicalCwd
			profile.Plan.Predicate = task.PredicateRef{Adapter: request.Provider, Mode: request.Mode, Version: "1", SHA256: task.ComputeSHA256([]byte("predicate"))}
			return profile, nil
		},
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: "/opt/provider/provider"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := FinalizeCandidate(candidate, request, facts)
	if err != nil {
		t.Fatalf("path-only runtime observation was refused: %v", err)
	}
	if profile.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{Executable: "/opt/provider/provider"}) || profile.ObservedVersion != "" {
		t.Fatalf("provider runtime = %+v version=%q, want the installed path only", profile.Plan.ProviderRuntime, profile.ObservedVersion)
	}
}

// TestFinalizeSavedFactsAcceptsJournalReusedAcrossProviderUpdate reuses a
// completed journal whose facts name the provider observed before an update.
// Adapter finalizers bind facts to the current executable, so finalizing the
// raw saved facts refuses; the canonical saved-facts step projects them first.
func TestFinalizeSavedFactsAcceptsJournalReusedAcrossProviderUpdate(t *testing.T) {
	const current = "/opt/provider/next/provider"
	request := task.TaskRecord{
		Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace",
		RequestedConfig: task.TaskConfig{Permission: "read-only", Budget: "1s"}, BudgetNanos: int64(time.Second),
	}
	definition := commonprovider.InspectionDefinition{Runtime: &commonprovider.RuntimeProbeDefinition{Executable: current}}
	candidate := commonprovider.ProfileCandidate{
		Directory: request.CanonicalCwd, Inspection: &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			facts, err := commonprovider.DecodeInspectionFacts(data)
			if err != nil || facts.Runtime == nil || facts.Runtime.Executable != current || facts.Runtime.SHA256 != "" {
				return commonprovider.PreparedProfile{}, commonprovider.ErrProfileUnavailable
			}
			profile := commonprovider.PreparedProfile{
				ObservedVersion: facts.Runtime.Version,
				Effective:       task.EffectiveConfig{Containment: "read-only", Approval: "never", Digest: task.ComputeSHA256([]byte("config"))},
			}
			profile.Plan.Executable, profile.Plan.Arguments, profile.Plan.Directory = current, []string{"-"}, request.CanonicalCwd
			profile.Plan.Predicate = task.PredicateRef{Adapter: request.Provider, Mode: request.Mode, Version: "1", SHA256: task.ComputeSHA256([]byte("predicate"))}
			return profile, nil
		},
	}
	saved, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: "/opt/provider/old/provider", Version: "provider-1", SHA256: strings.Repeat("a", 64),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = FinalizeCandidate(candidate, request, saved); err == nil {
		t.Fatal("fixture finalizer accepted facts for a different executable")
	}
	profile, err := finalizeSavedFacts(candidate, request, saved)
	if err != nil {
		t.Fatalf("reused journal was refused after a provider-only update: %v", err)
	}
	if profile.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{Executable: current}) {
		t.Fatalf("provider runtime = %+v, want the current path without the historical version or digest", profile.Plan.ProviderRuntime)
	}
}
