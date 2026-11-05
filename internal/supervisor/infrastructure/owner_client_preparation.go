package infrastructure

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func requestOwnerPreparation(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, request ownerRequest) (ownerReply, error) {
	if err := request.validate(); err != nil {
		return ownerReply{}, err
	}
	reply, contactErr := contactOwner(ctx, store, pin, request)
	if reply == (ownerReply{}) {
		return ownerReply{}, contactErr
	}
	return reply, errors.Join(contactErr, ownerReplyCause(reply.Code))
}
