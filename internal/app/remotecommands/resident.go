package remotecommands

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func (c Client) requireRemoteResident(ctx context.Context, root string) (resultErr error) {
	reader, err := c.Stores.Read(ctx, root, c.pin)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	process, err := infrastructure.ObserveOwnerProcess(ctx, reader)
	if err != nil {
		return errors.New("remote supervisor is not ready; run supervisor ensure-owner in a prepared persistent user context")
	}
	ref, err := process.Reference()
	if err != nil {
		return err
	}
	return lifetime.RequirePersistentProcess(ref)
}
