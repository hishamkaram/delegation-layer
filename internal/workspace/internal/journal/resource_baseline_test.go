package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func baselineForTest(resource *Resource, source Source, reservation Reservation, tree string) contract.TurnBaseline {
	return contract.TurnBaseline{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, Sequence: reservation.Sequence,
		Subject: reservation.Subject, BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("b", 40), TreeOID: tree,
		IndexSHA256: testSHA('d'), ObjectFormat: source.ObjectFormat, Ref: "refs/delegate/baselines/one",
	}
}

func TestTurnBaselineRequiresOpenObligatedTaskUseAndIsCreateOnce(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	source := testSource(t)
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	subject := Subject{Kind: SubjectTask, ID: testID('d'), SHA256: testSHA('e')}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	baseline := baselineForTest(resource, source, reservation, strings.Repeat("c", 40))
	if err = resource.RecordTurnBaseline(t.Context(), baseline); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("baseline without capture obligation error=%v, want evidence fault", err)
	}
	if err = resource.RecordCaptureObligation(t.Context(), reservation, Subject{Kind: SubjectTask, ID: testID('f'), SHA256: testSHA('f')}); err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordTurnBaseline(t.Context(), baseline); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("baseline for another obligated subject error=%v, want identity mismatch", err)
	}
	if _, err = resource.TurnBaseline(t.Context(), reservation.Sequence); !task.RecordAbsent(err) {
		t.Fatalf("refused baseline was published: %v", err)
	}
}

func TestTurnBaselineCreateOnceReplayConflictAndClosure(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	source := testSource(t)
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	subject := Subject{Kind: SubjectTask, ID: testID('d'), SHA256: testSHA('e')}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	baseline := baselineForTest(resource, source, reservation, strings.Repeat("c", 40))
	if err = resource.RecordTurnBaseline(t.Context(), baseline); err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordTurnBaseline(t.Context(), baseline); err != nil {
		t.Fatalf("matching replay was refused: %v", err)
	}
	conflict := baselineForTest(resource, source, reservation, strings.Repeat("e", 40))
	if err = resource.RecordTurnBaseline(t.Context(), conflict); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting baseline error=%v, want evidence fault", err)
	}
	stored, err := resource.TurnBaseline(t.Context(), reservation.Sequence)
	if err != nil || stored != baseline {
		t.Fatalf("stored baseline=%+v, %v; want original winner", stored, err)
	}
	inventory, err := resource.TurnBaselines(t.Context())
	if err != nil || len(inventory) != 1 || inventory[0] != baseline {
		t.Fatalf("baseline inventory=%+v, %v", inventory, err)
	}
	if err = resource.CloseReservation(t.Context(), reservation, "unavailable", ""); err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordTurnBaseline(t.Context(), baseline); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("baseline after closed use error=%v, want invalid permit", err)
	}
}

func TestTurnBaselineReadsNeverClassifyCorruptionAsAbsent(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	defer closeResource(t, resource)
	if err := resource.RecordSource(t.Context(), testSource(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.TurnBaseline(t.Context(), 1); !task.RecordAbsent(err) {
		t.Fatalf("absent legacy baseline error=%v, want authoritative absence", err)
	}
	path := filepath.Join(store.Root, "workspaces", request.WorkspaceID, turnBaselineName(1))
	original := []byte("{not-json}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resource.TurnBaseline(t.Context(), 1)
	if !errors.Is(err, task.ErrEvidenceFault) || task.RecordAbsent(err) {
		t.Fatalf("corrupt baseline error=%v, want evidence fault without absence", err)
	}
	if _, err = resource.TurnBaselines(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("corrupt baseline inventory error=%v, want evidence fault", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, original) {
		t.Fatalf("corrupt baseline was replaced: %q, %v", stored, err)
	}
}

func TestTurnBaselineInventoryRejectsMalformedFamilyName(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	if err := resource.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, err := tx.Put(turnBaselineRecordPrefix+"1.json", contract.TurnBaseline{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := resource.TurnBaselines(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("malformed baseline name error=%v, want evidence fault", err)
	}
}
