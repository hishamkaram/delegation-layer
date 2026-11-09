package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func scheduleHealth(ctx context.Context, store contract.Store, id string, response Response) commandResult {
	reliability, ok := store.(contract.ReliabilityStore)
	if !ok {
		return failed(response, errors.New("scheduler reliability storage unavailable"), 1)
	}
	health, err := reliability.Health(ctx, id, time.Now().UTC())
	if err != nil {
		return failed(response, err, 1)
	}
	response.ScheduleHealth = &health
	return commandResult{response: response}
}

func schedulePolicy(ctx context.Context, store contract.Store, a Arguments, response Response) commandResult {
	reliability, ok := store.(contract.ReliabilityStore)
	if !ok {
		return failed(response, errors.New("scheduler policy storage unavailable"), 1)
	}
	p, err := reliability.QuotaPolicy(ctx, a.ScheduleID)
	if err != nil {
		return failed(response, err, 1)
	}
	if a.SchedulePolicyOperation == "set" {
		switch a.ScheduleQuotaHold {
		case "on":
			p.Enabled = true
		case "off":
			p.Enabled = false
		case "":
		default:
			return failed(response, errors.New("quota-hold must be on or off"), 2)
		}
		if a.ScheduleQuotaMaxHold != "" {
			p.MaxHold, err = time.ParseDuration(a.ScheduleQuotaMaxHold)
			if err != nil {
				return failed(response, err, 2)
			}
		}
		if a.ScheduleQuotaHold == "" && a.ScheduleQuotaMaxHold == "" {
			return failed(response, errors.New("policy set requires --quota-hold or --quota-max-hold"), 2)
		}
		if err = contract.ValidateQuotaPolicy(p); err != nil {
			return failed(response, err, 2)
		}
		if err = reliability.SetQuotaPolicy(ctx, a.ScheduleID, p); err != nil {
			return failed(response, err, 1)
		}
		p, err = reliability.QuotaPolicy(ctx, a.ScheduleID)
		if err != nil {
			return failed(response, err, 1)
		}
	}
	response.SchedulePolicy = &p
	return commandResult{response: response}
}

func runSchedulerMigrate(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("scheduler")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	// Lock the host first: execution ownership can be momentarily free during
	// managed-host backoff, so the owner lock alone cannot authorize migration.
	controller, err := host.AcquireHostLease(ctx, store)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, controller.Close) }()
	lease, err := scheduler.AcquireOwnerLease(ctx, store)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, lease.Close) }()
	if err = lease.Migrate(ctx); err != nil {
		return failed(response, err, 1)
	}
	response.Scheduler = &SchedulerResponse{State: "migrated"}
	return commandResult{response: response}
}
