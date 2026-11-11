package tasklifecycle

import (
	"context"
	"errors"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func LoadContinuationInput(root string, a DispatchRequest, options DispatchOptions) (req *task.TaskRecord, meta *task.MetaRecord, brief []byte, resultErr error) {
	return LoadContinuationInputContext(context.Background(), root, a, options)
}

func LoadContinuationInputContext(ctx context.Context, root string, a DispatchRequest, options DispatchOptions) (req *task.TaskRecord, meta *task.MetaRecord, brief []byte, resultErr error) {
	req, meta, brief, err := loadContinuationPredecessorContext(ctx, root, a.TaskID, options)
	if err != nil {
		return req, meta, brief, err
	}
	brief, err = continuationBrief(a, brief)
	if err != nil {
		return nil, nil, nil, err
	}
	return req, meta, brief, nil
}

// The predecessor store owns record, ownership and inherited-brief validation.
// Join all owned closes before a caller override can be read or classified.
func loadContinuationPredecessorContext(ctx context.Context, root, id string, options DispatchOptions) (req *task.TaskRecord, meta *task.MetaRecord, brief []byte, resultErr error) {
	store, err := options.openStore(root, false)
	if err != nil {
		return nil, nil, nil, err
	}
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, nil, nil, errors.Join(err, store.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close(), store.Close()) }()
	req, meta, err = td.PreparedRecords()
	if err != nil {
		return nil, nil, nil, err
	}
	ownership, err := supervisorcore.RunnerOwnershipForTaskContext(ctx, root, td, meta)
	if err != nil {
		return nil, nil, nil, err
	}
	if meta != nil && meta.RunnerOwnership == "" && ownership == task.RunnerOwnershipManaged {
		migratedMeta := *meta
		migratedMeta.RunnerOwnership = task.RunnerOwnershipManaged
		meta = &migratedMeta
	}
	brief, err = td.ReadBrief()
	if err != nil {
		return nil, nil, nil, err
	}
	return req, meta, brief, nil
}

// An absent override inherits the already validated bytes. Explicit input uses
// the same source exclusivity and limits as ordinary dispatch.
func continuationBrief(request DispatchRequest, inherited []byte) ([]byte, error) {
	if request.BriefPath == "" && request.BriefBytes == nil {
		return inherited, nil
	}
	return ReadBriefInput(request.BriefPath, request.BriefBytes)
}

func loadContinuationPrior(root, id string, options DispatchOptions) (prior *task.PriorSession, source ContinuationSessionSource, resultErr error) {
	store, err := options.openStore(root, false)
	if err != nil {
		return nil, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	predecessor, _, err := td.PreparedRecords()
	if err != nil {
		return nil, "", err
	}
	return resolveContinuationSession(td, predecessor)
}
