package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

type infrastructureSavedBindingFixture struct {
	store   *taskdir.Store
	source  infrastructure.SavedBindingSource
	binding task.SupervisorRef
	slot    string
}

func newInfrastructureSavedBindingFixture(t *testing.T, kind infrastructure.SavedBindingKind, private bool) infrastructureSavedBindingFixture {
	t.Helper()
	if kind == infrastructure.SavedBindingWorkspaceRequest {
		return newInfrastructureWorkspaceBindingFixture(t, private)
	}
	store := supervisorfixture.NewInspectionStore(t)
	config := pueue.PrivateConfigPath(store.Root)
	if !private {
		config = "/external/pueue.yml"
	}
	td, _ := lifecyclefixture.NewAppTaskInStoreWithSupervisorConfig(t, store, strings.Repeat("c", 32), false, nil, config)
	t.Cleanup(func() {
		if err := td.Close(); err != nil {
			t.Error(err)
		}
	})
	_, meta, _, _, err := td.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	recordID, slot := "", filepath.Join(td.Dir, "meta.json")
	switch kind {
	case infrastructure.SavedBindingTaskSubmission:
		permit, prepareErr := td.PrepareSubmission(meta.SupervisorConfig)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
			t.Fatal(err)
		}
		slot = filepath.Join(td.Dir, "submit.json")
	case infrastructure.SavedBindingTaskStop:
		recordID = strings.Repeat("d", 32)
		permit, prepareErr := td.PrepareStop(recordID, "user", time.Time{})
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
			t.Fatal(err)
		}
		slot = filepath.Join(td.Dir, "stop", recordID+".request.json")
	case infrastructure.SavedBindingTaskMetadata:
	case infrastructure.SavedBindingWorkspaceRequest:
		t.Fatal("workspace fixture must use its operation reader")
	default:
		t.Fatal("unknown fixture source")
	}
	return makeInfrastructureSavedBindingFixture(t, store, infrastructure.SavedBindingSource{Kind: kind, OwnerID: td.TaskID, RecordID: recordID}, meta.SupervisorConfig, slot)
}

func makeInfrastructureSavedBindingFixture(t *testing.T, store *taskdir.Store, source infrastructure.SavedBindingSource, binding task.SupervisorRef, slot string) infrastructureSavedBindingFixture {
	t.Helper()
	digest, err := infrastructure.SavedBindingDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	source.RootID, source.BindingSHA256 = store.RootID, digest
	return infrastructureSavedBindingFixture{store: store, binding: binding, slot: slot, source: source}
}

func TestInfrastructureSavedBindingCanonicalSlots(t *testing.T) {
	for _, kind := range []infrastructure.SavedBindingKind{
		infrastructure.SavedBindingTaskMetadata, infrastructure.SavedBindingTaskSubmission,
		infrastructure.SavedBindingTaskStop, infrastructure.SavedBindingWorkspaceRequest,
	} {
		t.Run(string(kind), func(t *testing.T) {
			for _, state := range []string{"matching", "missing slot", "corrupt slot", "different binding", "different root", "external config", "cancelled"} {
				t.Run(state, func(t *testing.T) {
					checkInfrastructureSavedBindingCase(t, kind, state)
				})
			}
		})
	}
}

func newInfrastructureWorkspaceBindingFixture(t *testing.T, private bool) infrastructureSavedBindingFixture {
	t.Helper()
	store, operation := workspacefixture.NewOperation(t)
	if !private {
		request := operation.Request()
		request.OperationID = strings.Repeat("e", 32)
		request.Supervisor.ConfigPath = "/external/pueue.yml"
		var err error
		operation, err = workspace.OpenOperation(t.Context(), store, request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := operation.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	request := operation.Request()
	return makeInfrastructureSavedBindingFixture(t, store, infrastructure.SavedBindingSource{Kind: infrastructure.SavedBindingWorkspaceRequest, OwnerID: request.OperationID}, request.Supervisor,
		filepath.Join(store.Root, "workspace-operations", request.OperationID, "request.json"))
}

func checkInfrastructureSavedBindingCase(t *testing.T, kind infrastructure.SavedBindingKind, state string) {
	t.Helper()
	fixture := newInfrastructureSavedBindingFixture(t, kind, state != "external config")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture = changeInfrastructureSavedBindingCase(t, fixture, state)
	if state == "cancelled" {
		cancel()
	}
	before := supervisorfixture.SnapshotStore(t, fixture.store.Root)
	binding, err := resolveInfrastructureSavedBinding(ctx, fixture.store, fixture.source)
	if state == "matching" {
		if err != nil || binding != fixture.binding {
			t.Fatalf("canonical slot binding = %#v, %v", binding, err)
		}
	} else if err == nil || binding != (task.SupervisorRef{}) {
		t.Fatalf("invalid source accepted: binding %#v, error %v", binding, err)
	}
	if state == "cancelled" && !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if after := supervisorfixture.SnapshotStore(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("saved source resolution changed rooted state")
	}
}

func changeInfrastructureSavedBindingCase(t *testing.T, fixture infrastructureSavedBindingFixture, state string) infrastructureSavedBindingFixture {
	t.Helper()
	switch state {
	case "missing slot":
		if err := os.Remove(fixture.slot); err != nil {
			t.Fatal(err)
		}
	case "corrupt slot":
		writeFixtureFile(t, fixture.slot, []byte("{"))
	case "different binding":
		changed := fixture.binding
		changed.Endpoint = "unix:/other/socket"
		digest, err := infrastructure.SavedBindingDigest(changed)
		if err != nil {
			t.Fatal(err)
		}
		fixture.source.BindingSHA256 = digest
	case "different root":
		fixture.source.RootID = strings.Repeat("f", 32)
	}
	return fixture
}
