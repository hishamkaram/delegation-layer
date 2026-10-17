package tasklifecycle

import (
	"errors"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func LoadContinuationInput(root string, a DispatchRequest, options DispatchOptions) (req *task.TaskRecord, meta *task.MetaRecord, brief []byte, resultErr error) {
	store, err := options.openStore(root, false)
	if err != nil {
		return nil, nil, nil, err
	}
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return nil, nil, nil, errors.Join(err, store.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close(), store.Close()) }()
	req, meta, err = td.PreparedRecords()
	if err != nil {
		return nil, nil, nil, err
	}
	ownership, err := supervisorcore.RunnerOwnershipForTask(root, td, meta)
	if err != nil {
		return nil, nil, nil, err
	}
	if meta != nil && meta.RunnerOwnership == "" && ownership == task.RunnerOwnershipManaged {
		migratedMeta := *meta
		migratedMeta.RunnerOwnership = task.RunnerOwnershipManaged
		meta = &migratedMeta
	}
	brief, err = td.ReadBrief()
	if err == nil && a.BriefPath != "" {
		brief, err = ReadBrief(a.BriefPath)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return req, meta, brief, nil
}

func loadContinuationPrior(root, id string, options DispatchOptions) (prior *task.PriorSession, resultErr error) {
	store, err := options.openStore(root, false)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	predecessor, _, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	return ContinuationPrior(td, predecessor)
}
