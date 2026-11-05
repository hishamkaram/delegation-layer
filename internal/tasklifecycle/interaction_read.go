package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ReadInteraction(td *taskdir.TaskDir) (*task.InteractionState, error) {
	req, _, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	if !req.RequestedConfig.Interactive {
		return nil, nil
	}
	state, err := td.InteractionState()
	if err != nil {
		return nil, err
	}
	return &state, nil
}

func collectWithInteraction(ctx context.Context, td *taskdir.TaskDir, ref task.PredicateRef, observed *Observation) (*task.OutcomeRecord, error) {
	outcome, collectErr := CollectAttemptContext(ctx, td, ref)
	interaction, interactionErr := ReadInteraction(td)
	if interaction != nil {
		observed.Interaction = interaction
	}
	if outcome != nil {
		return outcome, collectErr
	}
	return nil, errors.Join(collectErr, interactionErr)
}
