package remotecommands

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remote/payload"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// PayloadReader uses the existing sealed-payload download protocol. No local
// output directory, reference mutation or publication recovery is involved.
// The returned reader retains no connection, file, pipe or goroutine: each
// Coordinator.Call owns and settles its exchange before Read returns.
func (c Client) PayloadReader(ctx context.Context, root, handle string, descriptor task.PayloadDescriptor) (io.Reader, error) {
	c = c.captureEnvironment()
	if err := task.ValidatePayloadDescriptor(descriptor); err != nil {
		return nil, err
	}
	id, err := remote.ParseHandle(handle)
	if err != nil {
		return nil, err
	}
	root, err = tasklifecycle.ResolveRoot(root)
	if err != nil {
		return nil, err
	}
	reader, err := c.Stores.Read(ctx, root, nil)
	if err != nil {
		return nil, err
	}
	intent, readErr := readRemoteIntent(ctx, reader, id)
	if err = errors.Join(readErr, reader.Close()); err != nil {
		return nil, err
	}
	configuration, err := loadSSHConfiguration(intent.SSHConfig)
	if err != nil {
		return nil, err
	}
	coordinator, err := c.recordedRemoteCoordinator(configuration, root, intent)
	if err != nil {
		return nil, err
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		return nil, err
	}
	request := remote.Request{Version: remote.ProtocolVersion, IntentSHA256: digest, Operation: remote.Download, Root: intent.Destination.Root, Expected: &intent.Destination, TaskID: id, Transfer: &remote.TransferRequest{Selector: "payload"}}
	response, err := coordinator.Call(ctx, request)
	if err != nil {
		return nil, err
	}
	expected := remote.Descriptor{Name: descriptor.Basename, Length: descriptor.Length, SHA256: descriptor.SHA256}
	if response.Transfer == nil || response.Transfer.Availability != "available" || response.Transfer.Descriptor != expected {
		return nil, remote.ErrProtocol
	}
	return &payloadReader{ctx: ctx, coordinator: coordinator, request: request, descriptor: expected}, nil
}

type payloadReader struct {
	ctx         context.Context
	coordinator remote.Coordinator
	request     remote.Request
	descriptor  remote.Descriptor
	offset      int64
}

func (r *payloadReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.offset == r.descriptor.Length {
		return 0, io.EOF
	}
	length := min(int64(len(buffer)), int64(remote.MaxDataBytes), r.descriptor.Length-r.offset)
	r.request.Transfer.Offset, r.request.Transfer.Length = r.offset, length
	response, err := r.coordinator.Call(r.ctx, r.request)
	if err != nil {
		return 0, err
	}
	if response.Transfer == nil || response.Transfer.Descriptor != r.descriptor || int64(len(response.Data)) != length || response.Transfer.Offset != r.offset+length {
		return 0, remote.ErrProtocol
	}
	n := copy(buffer, response.Data)
	r.offset += int64(n)
	return n, nil
}

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
