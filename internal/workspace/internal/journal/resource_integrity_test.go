package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestCaptureIDsIgnoresPurposeAndObligationRecords(t *testing.T) {
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
	operationID := testID('f')
	operation := OperationRequest{
		SchemaVersion: task.SchemaVersion,
		OperationID:   operationID,
		WorkspaceID:   resource.Request().WorkspaceID,
		Kind:          OperationCapture,
	}
	if _, err = resource.AssociateOperation(t.Context(), CapturePurpose(reservation.Sequence), operation); err != nil {
		t.Fatal(err)
	}
	candidate := CaptureCandidate{
		SchemaVersion: task.SchemaVersion,
		WorkspaceID:   resource.Request().WorkspaceID,
		OperationID:   operationID,
		Subject:       subject,
		BaseOID:       source.BaseOID,
		ObservedHEAD:  strings.Repeat("b", 40),
		TreeOID:       strings.Repeat("c", 40),
		SnapshotRef:   "refs/delegate/capture-" + operationID,
		ObjectFormat:  source.ObjectFormat,
	}
	if err = resource.RecordCaptureCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	receipt := CaptureReceipt{
		SchemaVersion:     task.SchemaVersion,
		WorkspaceID:       resource.Request().WorkspaceID,
		OperationID:       operationID,
		Subject:           subject,
		Status:            "captured",
		Candidate:         &candidate,
		TerminationSHA256: testSHA('g'),
		WorkerWaitSHA256:  testSHA('h'),
	}
	if err = resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if CapturePurpose(reservation.Sequence) == obligationName(reservation.Sequence) {
		t.Fatal("capture purpose and obligation share a record name")
	}

	ids, err := resource.CaptureIDs(t.Context())
	if err != nil {
		t.Fatalf("capture inventory failed with purpose and obligation records: %v", err)
	}
	if len(ids) != 1 || ids[0] != operationID {
		t.Fatalf("capture ids=%v, want [%s]", ids, operationID)
	}
}

func TestCaptureIDsRejectsMalformedNamesInOwnedRecordFamilies(t *testing.T) {
	cases := []struct {
		name    string
		receipt bool
	}{
		{name: captureCandidateRecordPrefix + "not-a-task-id.json"},
		{name: captureReceiptRecordPrefix + "not-a-task-id.json", receipt: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, resource, _ := newResourceFixture(t)
			defer closeResource(t, resource)
			if err := resource.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
				if testCase.receipt {
					_, err := tx.Put(testCase.name, CaptureReceipt{})
					return err
				}
				_, err := tx.Put(testCase.name, CaptureCandidate{})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := resource.CaptureIDs(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("malformed capture name error=%v, want evidence fault", err)
			}
		})
	}
}

func TestAssociatedOperationClassifiesCorruptPurposeWithoutReplacingIt(t *testing.T) {
	store, resource, request := newResourceFixture(t)
	defer closeResource(t, resource)
	purpose := CapturePurpose(1)
	purposePath := filepath.Join(store.Root, "workspaces", request.WorkspaceID, purpose)
	original := []byte("{not-json}\n")
	if err := os.WriteFile(purposePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	proposed := OperationRequest{SchemaVersion: task.SchemaVersion, OperationID: testID('a'), WorkspaceID: request.WorkspaceID, Kind: OperationCapture}
	if _, err := resource.AssociatedOperation(t.Context(), purpose); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("corrupt purpose read error=%v, want evidence fault", err)
	}
	if _, err := resource.AssociateOperation(t.Context(), purpose, proposed); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("corrupt purpose association error=%v, want evidence fault", err)
	}
	stored, err := os.ReadFile(purposePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, original) {
		t.Fatalf("corrupt purpose was replaced: %q", stored)
	}
}

func TestResourceRegistrationUsesAuthoritativeSource(t *testing.T) {
	t.Run("missing source", func(t *testing.T) {
		_, resource, _ := newResourceFixture(t)
		defer closeResource(t, resource)
		source := testSource(t)
		registration := resourceRegistrationForTest(resource, source)
		seedRegistrationForTest(t, resource, registration)
		if _, err := resource.Registration(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("missing source read error=%v, want evidence fault", err)
		}
		if err := resource.RecordRegistration(t.Context(), registration); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("missing source write error=%v, want evidence fault", err)
		}
	})

	t.Run("mismatching source", func(t *testing.T) {
		_, resource, _ := newResourceFixture(t)
		defer closeResource(t, resource)
		source := testSource(t)
		if err := resource.RecordSource(t.Context(), source); err != nil {
			t.Fatal(err)
		}
		registration := resourceRegistrationForTest(resource, source)
		registration.BaseOID = strings.Repeat("b", 40)
		if err := resource.RecordRegistration(t.Context(), registration); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("mismatching source write error=%v, want evidence fault", err)
		}
		seedRegistrationForTest(t, resource, registration)
		if _, err := resource.Registration(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("mismatching source read error=%v, want evidence fault", err)
		}
	})

	t.Run("corrupt source", testResourceRegistrationCorruptSource)
}

func testResourceRegistrationCorruptSource(t *testing.T) {
	t.Helper()
	store, resource, request := newResourceFixture(t)
	source := testSource(t)
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	registration := resourceRegistrationForTest(resource, source)
	if err := resource.RecordRegistration(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	closeResource(t, resource)
	if err := os.WriteFile(filepath.Join(store.Root, "workspaces", request.WorkspaceID, resourceSourceName), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := LoadResource(t.Context(), store, request.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResource(t, reopened)
	if _, err = reopened.Registration(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("corrupt source read error=%v, want evidence fault", err)
	}
	if err = reopened.RecordRegistration(t.Context(), registration); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("corrupt source write error=%v, want evidence fault", err)
	}
}

func TestTerminalReadersRequireAuthoritativeOperationIdentity(t *testing.T) {
	t.Run("removed", func(t *testing.T) {
		_, resource, _ := newResourceFixture(t)
		defer closeResource(t, resource)
		release := ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: testID('b')}
		if err := resource.RecordRelease(t.Context(), release); err != nil {
			t.Fatal(err)
		}
		mismatched := RemovedRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: testID('c')}
		seedRemovedForTest(t, resource, mismatched)
		if _, err := resource.Removed(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("mismatching removed read error=%v, want evidence fault", err)
		}
		if err := resource.RecordRemoved(t.Context(), release.OperationID); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("mismatching removed replay error=%v, want evidence fault", err)
		}
	})

	t.Run("forgotten", func(t *testing.T) {
		_, resource, _ := newResourceFixture(t)
		defer closeResource(t, resource)
		intent := ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: testID('d'), BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
		if err := resource.RecordForgetIntent(t.Context(), intent); err != nil {
			t.Fatal(err)
		}
		mismatched := ForgottenRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: testID('e')}
		seedForgottenForTest(t, resource, mismatched)
		if _, err := resource.Forgotten(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("mismatching forgotten read error=%v, want evidence fault", err)
		}
		if err := resource.RecordForgotten(t.Context(), intent.OperationID); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("mismatching forgotten replay error=%v, want evidence fault", err)
		}
	})
}

func resourceRegistrationForTest(resource *Resource, source Source) Registration {
	checkoutPath := filepath.Join(resource.Request().Container, resource.Request().WorkspaceID, "checkout")
	return Registration{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID,
		SourceRoot: source.Root, CommonDir: source.CommonDir, GitDir: source.GitDir,
		AdminDir: filepath.Join(checkoutPath, ".git"), AdminBacklink: filepath.Join(checkoutPath, ".git"),
		CheckoutPath: checkoutPath, CanonicalCwd: checkoutPath, RelativeCwd: source.RelativeCwd,
		BaseOID: source.BaseOID, ObjectFormat: source.ObjectFormat, BaseRef: "HEAD",
		ContainerIdentity: DirectoryIdentity{Device: 1, Inode: 2}, CheckoutIdentity: DirectoryIdentity{Device: 3, Inode: 4},
	}
}

func seedRegistrationForTest(t *testing.T, resource *Resource, registration Registration) {
	t.Helper()
	if err := resource.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, err := tx.Put(resourceRegistrationName, registration)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func seedRemovedForTest(t *testing.T, resource *Resource, record RemovedRecord) {
	t.Helper()
	if err := resource.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, err := tx.Put(resourceRemovedName, record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func seedForgottenForTest(t *testing.T, resource *Resource, record ForgottenRecord) {
	t.Helper()
	if err := resource.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, err := tx.Put(resourceForgottenName, record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
