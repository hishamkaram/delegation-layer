package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInteractiveContinuationUsesFreshCatalogCandidate(t *testing.T) {
	workspace := testsupport.CanonicalTempDir(t)
	root := testsupport.CanonicalTempDir(t)
	interpreter, err := predicate.Default().Resolve(task.FixturePredicateRef())
	if err != nil {
		t.Fatal(err)
	}
	var freshCalls, existingCalls int
	candidate := func(request task.TaskRecord) commonprovider.ProfileCandidate {
		return commonprovider.ProfileCandidate{
			Directory: request.CanonicalCwd,
			Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
				return commonprovider.PreparedProfile{Plan: execution.Plan{Directory: request.CanonicalCwd}}, nil
			},
		}
	}
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{
		Description: commonprovider.Description{
			ID:               config.ProviderFixture,
			SupportedModes:   []string{config.ModeReadOnly},
			SupportedOptions: []string{commonprovider.OptionContinuation, commonprovider.OptionInteractive},
			Continuation:     commonprovider.ContinuationNative,
			Interaction:      task.InteractionCapabilities{SupportedModes: []string{config.ModeReadOnly}, Clarification: true},
			ReadOnly:         commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
			Runtime:          commonprovider.RuntimeCapability{RequiredFlags: []string{"--fixture"}},
			Discoverable:     true,
		},
		Prepare: func(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			freshCalls++
			return candidate(request), nil
		},
		PrepareExisting: func(request task.TaskRecord, _ task.MetaRecord) (commonprovider.ProfileCandidate, error) {
			existingCalls++
			return candidate(request), nil
		},
		Interpreters: []predicate.Interpreter{interpreter},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := continuationDependencies(Dependencies{Catalog: catalog}, task.MetaRecord{})
	base := task.TaskRecord{
		Provider: config.ProviderFixture, Mode: config.ModeReadOnly, CanonicalCwd: workspace,
		TaskID: "0123456789abcdef0123456789abcdef", RootID: "fedcba9876543210fedcba9876543210",
		BriefLength: 1, BudgetNanos: int64(time.Minute),
		RequestedConfig: task.TaskConfig{Permission: config.ModeReadOnly},
		PriorSession:    &task.PriorSession{Provider: config.ProviderFixture, ConversationID: "session", PredecessorTaskID: "abcdef0123456789abcdef0123456789"},
	}

	interactive := base
	interactive.RequestedConfig.Interactive = true
	if _, _, err := admissionProfiles(deps).ExistingContext(context.Background(), root, interactive, task.MetaRecord{}); err != nil {
		t.Fatal(err)
	}
	if freshCalls != 1 || existingCalls != 0 {
		t.Fatalf("interactive continuation candidate calls: fresh=%d existing=%d", freshCalls, existingCalls)
	}

	if _, _, err := admissionProfiles(deps).ExistingContext(context.Background(), root, base, task.MetaRecord{}); err != nil {
		t.Fatal(err)
	}
	if freshCalls != 1 || existingCalls != 1 {
		t.Fatalf("headless continuation candidate calls: fresh=%d existing=%d", freshCalls, existingCalls)
	}
}
