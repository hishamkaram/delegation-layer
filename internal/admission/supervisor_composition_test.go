package admission

import (
	"context"
	"sync/atomic"
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func newTerminalEventInspectionSupervisor(t *testing.T, store *taskdir.Store) (*pueue.Client, *atomic.Int32) {
	t.Helper()
	return admissionfixture.NewTerminalEventInspectionSupervisor(t, store, TerminalEvidence{})
}

func newTerminalEventInspectionSupervisorWithEnvironment(t *testing.T, store *taskdir.Store, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return admissionfixture.NewTerminalEventInspectionSupervisorWithEnvironment(t, store, TerminalEvidence{}, extraEnvironment...)
}

func newTerminalEventInspectionSupervisorWithLauncher(t *testing.T, store *taskdir.Store, launcher func(context.Context, string, string) error, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return admissionfixture.NewTerminalEventInspectionSupervisorWithLauncher(t, store, TerminalEvidence{}, launcher, extraEnvironment...)
}

func newTerminalInspectionCleanupSupervisor(t *testing.T, store *taskdir.Store) (*pueue.Client, string, string) {
	t.Helper()
	return admissionfixture.NewTerminalInspectionCleanupSupervisor(t, store, TerminalEvidence{})
}

func newInspectionTestCleanup() *supervisorcore.Cleanup {
	return supervisorcore.NewCleanup(TerminalEvidence{}, workspacescore.InspectPrivateOperation)
}
