package testsupport

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type QueuedInspectionUpgradeFixture struct {
	Store       *taskdir.Store
	Supervisor  *pueue.Client
	Request     task.TaskRecord
	Binding     inspection.Binding
	Replacement pueue.RunnerCommandReplacement
	OldRunner   string
	LogPath     string
}

func NewQueuedInspectionUpgradeFixture(t *testing.T) QueuedInspectionUpgradeFixture {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	supervisor, logPath := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	oldSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, oldSource, "old inspection worker")
	oldRunner, err := supervisorcore.InstallStateRunner(store.Root, oldSource)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commonprovider.FingerprintExecutable(oldRunner)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/usr/bin/true", HelperSHA256: strings.Repeat("a", 64),
		WorkerExecutable: oldRunner, WorkerSHA256: digest,
		RunnerOwnership: task.RunnerOwnershipManaged,
		Supervisor:      supervisor.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	binding := operation.Request().Binding
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}
	label := identity.Label()
	job := pueue.Job{ID: 17, Label: &label, Group: identity.Group(), State: pueue.StateQueued}
	replacement, eligible, err := supervisorcore.PrepareQueuedInspectionRunnerCommand(context.Background(), store, supervisor, job, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("managed inspection worker was not eligible for queue migration")
	}
	newSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, newSource, "new inspection worker")
	replacement.NewRunner, err = supervisorcore.InstallStateRunner(store.Root, newSource)
	if err != nil {
		t.Fatal(err)
	}
	return QueuedInspectionUpgradeFixture{
		Store: store, Supervisor: supervisor, Request: request,
		Binding: binding, Replacement: replacement, OldRunner: oldRunner, LogPath: logPath,
	}
}
