package tasklifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ResumeSupervisedRunner consumes the dispatcher's durable admission receipt.
// The supervisor already started this process, so bootstrapping it again or
// scanning its full queue cannot add authority and creates avoidable fan-out
// when many tasks start together.
func ResumeSupervisedRunner(ctx context.Context, response *Observation, td *taskdir.TaskDir, supervisorOptions pueue.Options) (*pueue.Client, pueue.Observation, error) {
	receipt, err := WaitRunnerSupervisorReceipt(ctx, td, pueue.PrivateControlTimeout(supervisorOptions))
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	client, err := pueue.NewClient(receipt.SupervisorRef(), supervisorOptions)
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	numericTaskID := receipt.NumericTaskID
	observation := pueue.Observation{
		Matched: true,
		State:   pueue.StateRunning,
		Identity: pueue.Identity{
			RootID:        receipt.RootID,
			TaskID:        receipt.TaskID,
			SpecSHA256:    receipt.SpecSHA256,
			MetaSHA256:    receipt.MetaSHA256,
			NumericTaskID: &numericTaskID,
		},
		Binding:       receipt.SupervisorRef(),
		NumericTaskID: numericTaskID,
	}
	response.Supervisor = DescribeSupervisor(observation)
	response.LivenessObserved, response.AdmissionObserved = true, true
	response.Admission = task.AdmissionAdmitted
	response.Liveness = LivenessFromQueue(observation.State)
	response.Pending = ""
	return client, observation, nil
}

func WaitRunnerSupervisorReceipt(parent context.Context, td *taskdir.TaskDir, timeout time.Duration) (*task.SupervisorReceipt, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	for {
		receipt, err := td.ReadSupervisorReceipt()
		if err == nil {
			return receipt, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		timer := time.NewTimer(RunnerReceiptPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("%w: supervisor receipt unavailable: %w", pueue.ErrUnknown, ctx.Err())
		case <-timer.C:
		}
	}
}

func SupervisorContextForRunner(req *task.TaskRecord, options pueue.Options) (context.Context, context.CancelFunc) {
	controlTimeout := pueue.PrivateControlTimeout(options)
	if budget := time.Duration(req.BudgetNanos); budget > controlTimeout {
		controlTimeout = budget
	}
	return context.WithTimeout(context.Background(), controlTimeout)
}
