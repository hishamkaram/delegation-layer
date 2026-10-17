package app

import (
	"errors"
	"os"
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/guardian"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOptionalInspectionLifetimeRecordDoesNotIgnoreMarkedDependency(t *testing.T) {
	record, err := guardian.OptionalRegistration(nil, errors.Join(task.ErrIdentityMismatch, os.ErrNotExist))
	if record != nil {
		t.Fatal("marked dependency fault returned a record")
	}
	if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("optional registration error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func recordInspectionLifetimeRegistration(t *testing.T, operation *inspection.Operation, bootID, assurance string) {
	t.Helper()
	admissionfixture.RecordInspectionLifetimeRegistration(t, operation, bootID, assurance)
}
