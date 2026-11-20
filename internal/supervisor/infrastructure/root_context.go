package infrastructure

import (
	"context"
	"errors"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// FindHistoricalRootContext finds a saved token's exact native preimage in this
// root's validated startup history. It neither adopts that history as the live
// process generation nor proves that no other copy of the root exists.
func FindHistoricalRootContext(ctx context.Context, store *taskdir.ReadOnlyStore, expectedToken string) (coordinates task.RootContextCoordinates, found bool, resultErr error) {
	if err := task.ValidateSHA256(expectedToken); err != nil {
		return coordinates, false, err
	}
	resultErr = withPassiveOwnerRecords(ctx, store, func(reader ownerRecordReader, pin taskdir.RootPin) error {
		var err error
		coordinates, found, err = findHistoricalRootContext(ctx, reader, pin, expectedToken)
		return err
	})
	if resultErr != nil {
		return task.RootContextCoordinates{}, false, resultErr
	}
	return coordinates, found, nil
}

func findHistoricalRootContext(ctx context.Context, reader ownerRecordReader, pin taskdir.RootPin, expectedToken string) (coordinates task.RootContextCoordinates, found bool, resultErr error) {
	names, err := reader.names(ctx)
	if task.RecordAbsent(err) {
		return coordinates, false, nil
	}
	if err != nil {
		return coordinates, false, err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "intent-") {
			continue
		}
		candidate, token, err := readHistoricalRootContext(ctx, reader, pin, name)
		if err != nil {
			return coordinates, false, err
		}
		if token != expectedToken {
			continue
		}
		if found && coordinates != candidate {
			return coordinates, false, task.ErrEvidenceFault
		}
		coordinates, found = candidate, true
	}
	return coordinates, found, ctx.Err()
}

func readHistoricalRootContext(ctx context.Context, reader ownerRecordReader, pin taskdir.RootPin, name string) (coordinates task.RootContextCoordinates, token string, resultErr error) {
	sequence, err := intentSequence(name)
	if err != nil {
		return coordinates, "", err
	}
	intent, err := readPinnedIntent(ctx, reader, pin, sequence)
	if err != nil {
		return coordinates, "", errors.Join(task.ErrEvidenceFault, err)
	}
	coordinates = task.RootContextCoordinates{Device: intent.Inputs.RootDevice, Inode: intent.Inputs.RootInode}
	token, err = task.ConnectorRootToken(pin.RootID(), coordinates)
	return coordinates, token, err
}
