package infrastructure

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func contextForJournal(t *testing.T, journal *intentJournal) LaunchContext {
	t.Helper()
	record := launchContextFixture(t)
	record.Root, record.RootID = journal.pin.Root(), journal.pin.RootID()
	return record
}

func TestInfrastructureContextStagingDoesNotRequireResidentLease(t *testing.T) {
	journal, _ := journalFixture(t)
	other, acquired, err := journal.tryAcquire(t.Context())
	if other != nil || acquired || err != nil {
		t.Fatalf("fixture resident lease is not held: %v %v", acquired, err)
	}
	record := contextForJournal(t, journal)
	reference, created, err := journal.stageContext(t.Context(), record)
	if err != nil || !created {
		t.Fatalf("immutable content staging blocked by resident: %v %v", created, err)
	}
	replay, created, err := journal.stageContext(t.Context(), record)
	if err != nil || created || replay != reference {
		t.Fatalf("idempotent content changed: %v %v", created, err)
	}
	got, err := journal.readContext(t.Context(), reference)
	if err != nil || !slices.Equal(got.ControlEnvironment, record.ControlEnvironment) || got.MaintenanceEnvironment == nil {
		t.Fatalf("staged snapshots changed: %v", err)
	}
	got.ControlEnvironment[0] = "HOME=/observer"
	requirePassiveContextSnapshot(t, journal, reference, record)
}

func requirePassiveContextSnapshot(t *testing.T, journal *intentJournal, reference LaunchContextReference, record LaunchContext) {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), journal.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	passive, err := readLaunchContext(t.Context(), reader, reference)
	if err != nil || !slices.Equal(passive.ControlEnvironment, record.ControlEnvironment) {
		t.Fatalf("passive read substituted observer environment: %v", err)
	}
}

func TestInfrastructureContextRefusesForeignOrChangedContent(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	foreign := record.Clone()
	foreign.RootID = strings.Repeat("e", 32)
	if _, created, err := journal.stageContext(t.Context(), foreign); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign context staged: %v %v", created, err)
	}
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	name, err := contextName(reference)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(journal.pin.Root(), "infrastructure", name)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(before, []byte("preserved"), []byte("different"), 1)
	if bytes.Equal(before, changed) {
		t.Fatal("fixture did not change environment bytes")
	}
	if err = os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = journal.readContext(t.Context(), reference); !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed snapshot accepted: %v", err)
	}
	if _, created, stageErr := journal.stageContext(t.Context(), record); created || !errors.Is(stageErr, task.ErrEvidenceFault) {
		t.Fatalf("conflicting content winner accepted: %v %v", created, stageErr)
	}
}

func TestInfrastructureContextMalformedReadRedactsValues(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	name, err := contextName(reference)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(journal.pin.Root(), "infrastructure", name)
	if err = os.WriteFile(path, []byte(`{"private-value-must-not-leak":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = journal.readContext(t.Context(), reference); err == nil || strings.Contains(err.Error(), "private-value-must-not-leak") {
		t.Fatalf("malformed context diagnostics leaked values: %v", err)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), journal.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = readLaunchContext(t.Context(), reader, reference); !errors.Is(err, task.ErrEvidenceFault) || strings.Contains(err.Error(), "private-value-must-not-leak") {
		t.Fatalf("malformed passive context lost fault or leaked values: %v", err)
	}
}
