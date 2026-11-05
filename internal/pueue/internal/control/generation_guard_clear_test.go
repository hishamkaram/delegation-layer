package control

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateGenerationGuardStaleAliasClearRefusesWithoutGrant(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	if err := fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, fixture.root)
	denied := errors.New("resident refused stale alias clear")
	calls := 0
	fixture.client.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, root string, _ func() error) error {
		calls++
		if root != fixture.root {
			t.Error("clear used a different resident operation root")
		}
		return denied
	})
	if err := fixture.client.ClearPrivateRecoveryBindingWithLock(t.Context(), fixture.root, fixture.client.Binding()); !errors.Is(err, denied) || calls != 1 {
		t.Fatalf("stale alias clear bypassed grant: calls=%d err=%v", calls, err)
	}
	if err := ClearPrivateRecoveryBinding(fixture.root, fixture.client.Binding()); !errors.Is(err, ErrInfrastructureOwnerRequired) {
		t.Fatalf("optionless clear acquired removal authority: %v", err)
	}
	if err := ClearPrivateRecoveryBindingIfPresent(fixture.root); !errors.Is(err, ErrInfrastructureOwnerRequired) {
		t.Fatalf("optionless broad clear acquired removal authority: %v", err)
	}
	if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
		t.Fatal("refused stale alias clear changed retained recovery state")
	}
}

func TestPrivateGenerationGuardStaleAliasClearComparesInsideCommit(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	if err := fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root); err != nil {
		t.Fatal(err)
	}
	record, found, readErr := readPrivateRecoveryRecord(fixture.root)
	if readErr != nil || !found {
		t.Fatalf("read recovery alias: found=%t err=%v", found, readErr)
	}
	replacement := record
	replacement.Supervisor.ConfigDigest = strings.Repeat("b", 64)
	calls := 0
	fixture.client.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		calls++
		data, err := task.MarshalCanonical(replacement)
		if err != nil {
			return err
		}
		if err := replacePrivateRecord(fixture.base, privateRecoveryRecordPath(fixture.root), data); err != nil {
			return err
		}
		return commit()
	})
	if err := fixture.client.ClearPrivateRecoveryBindingWithLock(t.Context(), fixture.root, fixture.client.Binding()); !errors.Is(err, ErrBinding) || calls != 1 {
		t.Fatalf("clear ignored changed identity inside grant: calls=%d err=%v", calls, err)
	}
	retained, found, err := readPrivateRecoveryRecord(fixture.root)
	if err != nil || !found || retained != replacement {
		t.Fatalf("clear removed another recovery identity: found=%t err=%v", found, err)
	}
	if _, err := os.Stat(record.ConfigPath); err != nil {
		t.Fatalf("clear removed another identity's config: %v", err)
	}
}

func TestPrivateGenerationGuardAbsentAliasClearRemainsWarm(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	fixture.client.options.PrivateGenerationGuard = nil
	if err := fixture.client.ClearPrivateRecoveryBindingWithLock(t.Context(), fixture.root, fixture.client.Binding()); err != nil {
		t.Fatalf("absent alias clear required a resident grant: %v", err)
	}
	if err := ClearPrivateRecoveryBinding(fixture.root, fixture.client.Binding()); err != nil {
		t.Fatalf("absent optionless alias clear required a grant: %v", err)
	}
	if err := ClearPrivateRecoveryBindingIfPresent(fixture.root); err != nil {
		t.Fatalf("absent broad alias clear required a grant: %v", err)
	}
}
