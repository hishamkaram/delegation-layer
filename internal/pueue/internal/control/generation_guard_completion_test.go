package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPrivateGenerationGuardRetirementRunsAfterQuiescence(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	if err := commitPrivateDaemonIdentity(fixture.base, fixture.client.Binding()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, fixture.root)
	denied := errors.New("resident revoked retirement")
	quiescent := false
	calls := 0
	guard := generationGuardFunc(func(_ context.Context, _ string, _ func() error) error {
		calls++
		if !quiescent {
			t.Error("retirement grant entered before quiescence authorization")
		}
		return denied
	})
	err := retirePrivateRecoveryBinding(t.Context(), fixture.root, fixture.client.Binding(), func(context.Context) error {
		quiescent = true
		return nil
	}, guard)
	if !errors.Is(err, denied) || !quiescent || calls != 1 {
		t.Fatalf("retirement bypassed grant or quiescence: calls=%d quiescent=%t err=%v", calls, quiescent, err)
	}
	if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
		t.Fatal("denied retirement removed recovery authority")
	}
}

func TestPrivateGenerationGuardCompletionUsesOneCommitAndExactReplay(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	record := completedGenerationGuardRecord(fixture)
	if err := publishPrivateMigrationState(fixture.base, record); err != nil {
		t.Fatal(err)
	}
	calls := 0
	inside := false
	fixture.client.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		calls++
		if inside {
			t.Fatal("completion nested the non-reentrant generation grant")
		}
		inside = true
		defer func() { inside = false }()
		return commit()
	})
	if err := fixture.client.commitPrivateMigrationCompletion(t.Context(), fixture.root, fixture.base, record); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("completion used %d grants, want one identity/history/clear transaction", calls)
	}
	assertGenerationCompletionRecords(t, fixture, record)
	// Recreate only the interrupted compare-and-remove step after the whole
	// completion fact is durable. This cleanup must work without a live grant.
	if err := publishPrivateMigrationState(fixture.base, record); err != nil {
		t.Fatal(err)
	}
	fixture.client.options.PrivateGenerationGuard = nil
	if err := fixture.client.commitPrivateMigrationCompletion(t.Context(), fixture.root, fixture.base, record); err != nil {
		t.Fatalf("exact completed-history replay required a grant: %v", err)
	}
	assertGenerationCompletionRecords(t, fixture, record)
}

func assertGenerationCompletionRecords(t *testing.T, fixture generationGuardFixture, record privateMigrationRecord) {
	t.Helper()
	if found, err := PrivateSupervisorMigrationPending(fixture.root); err != nil || found {
		t.Fatalf("completion retained migration intent: found=%t err=%v", found, err)
	}
	if found, err := PrivateBootstrapPending(fixture.root); err != nil || found {
		t.Fatalf("completion retained bootstrap intent: found=%t err=%v", found, err)
	}
	history, found, err := loadPrivateMigrationHistory(fixture.base)
	if err != nil || !found || len(history.Migrations) != 1 || history.Migrations[0] != record {
		t.Fatalf("completion lost its exact historical edge: found=%t err=%v", found, err)
	}
	published, found, err := loadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || published != fixture.client.Binding() {
		t.Fatalf("completion did not publish its exact successor: found=%t err=%v", found, err)
	}
}

func TestPrivateGenerationGuardWarmStartAllowsDerivedCleanup(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	if err := commitPrivateDaemonIdentity(fixture.base, fixture.client.Binding()); err != nil {
		t.Fatal(err)
	}
	if err := markPrivateBootstrapPending(fixture.root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.base, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.client.options.PrivateGenerationGuard = nil
	if err := WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		return fixture.client.StartPrivate(ctx, fixture.root)
	}); err != nil {
		t.Fatalf("warm read and identity-derived cleanup required a generation grant: %v", err)
	}
	if pending, err := PrivateBootstrapPending(fixture.root); err != nil || pending {
		t.Fatalf("warm identity did not clear derived bootstrap residue: pending=%t err=%v", pending, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.base, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("warm read started a daemon: %v", err)
	}
}
