package remotecommands

import (
	"context"
	"errors"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remote/payload"
)

func (c Client) transferRemote(ctx context.Context, request remote.Request, response remote.Response) remote.Response {
	if request.Transfer == nil || request.TaskID == "" {
		response.Failure = remote.FailureResponse("invalid_transfer", remote.ErrProtocol)
		return response
	}
	store, err := c.Stores.Open(ctx, request.Root, false, c.pin)
	if err != nil {
		response.Failure = remote.FailureResponse("state_unavailable", err)
		return response
	}
	response, err = payload.Stage(ctx, store, request, response)
	err = errors.Join(err, store.Close())
	if err != nil {
		response.Failure = remote.FailureResponse("transfer_failed", err)
	}
	return response
}

func (c Client) downloadRemote(ctx context.Context, request remote.Request, response remote.Response) (result remote.Response) {
	reader, err := c.Stores.Read(ctx, request.Root, c.pin)
	if err != nil {
		response.Failure = remote.FailureResponse("state_unavailable", err)
		return response
	}
	defer func() {
		if err := reader.Close(); err != nil {
			result.Diagnostics = append(result.Diagnostics, "download reader close failed: "+err.Error())
		}
	}()
	if request.Transfer != nil && strings.HasPrefix(request.Transfer.Selector, "log:") {
		return remoteLogDownload(ctx, reader, request, response)
	}
	return payload.Download(ctx, reader, request, response)
}
