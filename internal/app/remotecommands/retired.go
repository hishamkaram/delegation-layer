package remotecommands

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remote/payload"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// RetiredAdmission proves an already admitted identity before projecting facts
// without entering workspace validation or recovery. The adapter retains full
// error projection, including concrete faults joined to a retired cwd failure.
func (c Client) RetiredAdmission(ctx context.Context, pin taskdir.RootPin, request remote.Request, response remote.Response, cause error) (result remote.Response, retired bool, resultErr error) {
	reader, err := c.Stores.Read(ctx, request.Root, &pin)
	if err != nil {
		return response, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	stored, err := reader.ReadTaskRequest(ctx, request.TaskID)
	if task.RecordAbsent(err) {
		return response, false, nil
	}
	if err != nil {
		return response, false, err
	}
	_, present, err := payload.ReadRetirement(ctx, reader, request.TaskID, remotedata.Destination)
	if err != nil || !present {
		return response, false, err
	}
	if stored.Remote == nil || request.IntentSHA256 == "" || stored.Remote.IntentSHA256 != request.IntentSHA256 {
		return response, false, remote.ErrIdentity
	}
	closed, err := reader.RemoteTaskClosed(ctx, request.TaskID)
	if err != nil {
		return response, false, err
	}
	if !closed {
		return response, false, remote.ErrAcceptanceUnknown
	}
	response.TaskRecord = "created"
	response.Existing = true
	response.RequestSHA256 = request.IntentSHA256
	response.Diagnostics = append(response.Diagnostics, "destination_retired")
	// Settle this reader before projection: cleanup cannot downgrade proved
	// acceptance to the caller's earlier not_created response.
	cause = errors.Join(cause, reader.Close())
	return c.Local.Retired(ctx, pin, request, response, cause), true, nil
}
