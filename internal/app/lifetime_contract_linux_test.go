//go:build linux

package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestLifetimeGuardianPersistsIntentBeforeNativeCgroupCreation(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, request := newAppTaskInStore(t, store, lifetimeGuardianTestTaskID, false, nil)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })

	plan, err := lifetime.Prepare(request.RootID, task.LifetimeOwnerTask, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind() != lifetime.KindCgroupV2 {
		t.Skipf("cgroup v2 delegation unavailable; selected %s", plan.Kind())
	}

	var persistedIntentRead atomic.Bool
	deadline := time.Now().Add(time.Minute).UTC()
	fixture := newProductionRecordGuardianFixture(
		t,
		request.RootID,
		request.TaskID,
		task.LifetimeOwnerTask,
		deadline,
		td,
		func(ctx context.Context) (lifetimeGuardianOwner, error) {
			return td.AcquireLifetimeOwner(ctx)
		},
		func(options *lifetimeGuardianOptions) {
			options.planFactory = func(string, string, string) (lifetime.Plan, error) {
				return plan, nil
			}
			options.scopeFactory = func(lifetime.Plan) (lifetimeGuardianScope, error) {
				intent, readErr := td.ReadLifetimeIntent()
				if readErr != nil {
					return nil, fmt.Errorf("read persisted lifetime intent before native creation: %w", readErr)
				}
				persistedPlan, planErr := lifetime.NewPlan(
					lifetime.Kind(intent.ScopeKind),
					lifetime.Assurance(intent.Assurance),
					intent.ScopePlan,
				)
				if planErr != nil {
					return nil, fmt.Errorf("reconstruct persisted lifetime plan: %w", planErr)
				}
				if persistedPlan.Kind() != plan.Kind() ||
					persistedPlan.Assurance() != plan.Assurance() ||
					persistedPlan.Identifier() != plan.Identifier() {
					return nil, fmt.Errorf("persisted lifetime plan changed before native creation")
				}
				persistedIntentRead.Store(true)
				return lifetime.CreatePrepared(persistedPlan)
			}
		},
	)

	callProductionRecordGuardian(t, fixture, request.TaskID, lifetimeMessageHello)
	callProductionRecordGuardian(t, fixture, request.TaskID, lifetimeMessageClose)
	fixture.worker.release()
	if runErr := fixture.waitRun(t); runErr != nil {
		t.Fatalf("guardian with native cgroup: %v", runErr)
	}
	if !persistedIntentRead.Load() {
		t.Fatal("native cgroup was not created from the persisted lifetime intent")
	}
	ready, err := td.StopTerminationReady()
	if err != nil || !ready {
		t.Fatalf("native cgroup stop consumer = ready %t, err %v", ready, err)
	}
}
