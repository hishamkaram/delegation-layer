package supervisor

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Only completed canonical repair receipts supply alternative old commands.
// Merely placing an executable in the state tree cannot authorize replacement.
func recordedRepairRunnerPaths(ctx context.Context, store *taskdir.Store) (_ []string, resultErr error) {
	directory, err := store.OpenRunnerRepairContext(ctx, false)
	if task.RecordAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	names, err := directory.RecordNamesContext(ctx)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, "r-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		var receipt runnerRepairReceipt
		if err = directory.Read(name, &receipt); err != nil {
			return nil, err
		}
		if err = validateRunnerRepairReceiptShape(store.Root, receipt); err != nil {
			return nil, err
		}
		data, marshalErr := task.MarshalCanonical(receipt)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if name != "r-"+task.ComputeSHA256(data)[:48]+".json" {
			return nil, task.ErrEvidenceFault
		}
		paths = append(paths, receipt.RunnerExecutable)
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}
