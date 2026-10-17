package app

import (
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newInspectionPollTestOperation(t *testing.T, store *taskdir.Store, client *pueue.Client) *inspection.Operation {
	t.Helper()
	return admissionfixture.NewInspectionPollTestOperation(t, store, client)
}

func completeInspectionPollTestLifetime(t *testing.T, operation *inspection.Operation, cause string) {
	t.Helper()
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, cause)
}

func writeInspectionPollTestStatus(t *testing.T, path string, operation *inspection.Operation, failed bool) {
	t.Helper()
	admissionfixture.WriteInspectionPollTestStatus(t, path, operation, failed)
}
