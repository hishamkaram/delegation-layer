package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInfrastructurePreparationCommitUsesGrantWithoutRecordingStart(t *testing.T) {
	fixture := newInfrastructurePreparationFixture(t)
	guardCalls, commits := 0, 0
	guard := infrastructurePreparationGuard(func(ctx context.Context, root string, commit func() error) error {
		guardCalls++
		if ctx != t.Context() || root != fixture.origin.Root() {
			t.Fatal("commit grant lost the accepted context or root")
		}
		return commit()
	})
	_, wait := inertInfrastructurePreparationPorts()
	options, observation, err := infrastructurePreparationOptions(fixture.origin, fixture.artifacts, fixture.selected, guard, wait)
	if err != nil {
		t.Fatal(err)
	}
	if options.PrivateGenerationGuard == nil {
		t.Fatal("resident options omitted the generation commit grant")
	}
	if err = options.PrivateGenerationGuard.GuardPrivateGenerationCommit(t.Context(), fixture.origin.Root(), func() error { commits++; return nil }); err != nil {
		t.Fatal(err)
	}
	if guardCalls != 1 || commits != 1 || observation.Disposition() != (infrastructure.Disposition{}) {
		t.Fatal("publication was not granted exactly once or claimed native Start/readiness")
	}
}

func TestInfrastructurePreparationCleanupRetainsExactOwner(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	lease := appCleanupLifetimeLease(t, store)
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := lease.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fault := errors.New("resident admission release failed")
	store.SetFaultInjector(&workspaceAdmissionUnlockFault{cause: fault})
	owner := &infrastructurePreparationOwner{store: store}
	cleanupErr := owner.close(lease.ReleaseWithError(nil))
	var retained *infrastructurePreparationCleanupError
	if !errors.Is(cleanupErr, fault) || !errors.As(cleanupErr, &retained) || retained.owner != owner {
		t.Fatalf("resident lost its exact cleanup owner: %v", cleanupErr)
	}
	cleanupErr = closeGuardianStoreAfterCleanup(store, cleanupErr)
	if errors.Is(cleanupErr, task.ErrLockBusy) {
		t.Fatal("finalization attempted Store.Close behind unresolved admission cleanup")
	}
	var marker interface{ RetainsResidentCleanup() }
	if !errors.As(cleanupErr, &marker) {
		t.Fatal("resident server cannot retain the cleanup-owning error")
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("resident closed the borrowed Store: %v", err)
	}
	store.SetFaultInjector(nil)
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRecognizesMalformedInfrastructureOwnerBeforeTaskArguments(t *testing.T) {
	for _, args := range [][]string{{"--infrastructure-owner"}, {"--infrastructure-owner=invalid"}, {"--root", "/", "--infrastructure-owner"}} {
		var stderr bytes.Buffer
		code := RunRunner(args, io.Discard, &stderr, Dependencies{})
		if code != 1 || !strings.Contains(stderr.String(), "Infrastructure owner unavailable.") {
			t.Fatalf("owner mode fell through to task argument parsing: code %d, %q", code, stderr.String())
		}
	}
}
