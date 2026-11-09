package remotecommands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remote/payload"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type StorageInput struct {
	Action string
	TaskID string
	Scope  string
	Apply  bool
}

func (c Client) Storage(ctx context.Context, root string, input StorageInput) (report Report, resultErr error) {
	c = c.captureEnvironment()
	report.TaskID, report.Connectivity = input.TaskID, "not_connected"
	if input.Scope == "" {
		input.Scope = "both"
	}
	if err := input.validate(); err != nil {
		return report, err
	}
	id, err := remote.ParseHandle(input.TaskID)
	if err != nil {
		return report, err
	}
	resolved, err := tasklifecycle.ResolveRoot(root)
	if err != nil {
		return report, err
	}
	root = resolved
	reader, err := c.Stores.Read(ctx, root, nil)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if input.Action == "usage" {
		report.Usage = make(map[string]payload.UsageReport)
	} else {
		report.Retirement = make(map[string]payload.RetirementReport)
	}
	if input.Scope != "destination" {
		resultErr = c.storageOrigin(ctx, reader, id, input, &report)
	}
	if input.Scope == "origin" {
		return report, resultErr
	}
	intent, err := readRemoteIntent(ctx, reader, id)
	if err != nil {
		return report, errors.Join(resultErr, err)
	}
	response, err := c.remoteStorageRequest(ctx, root, intent, input)
	report.Remote = &response
	if response.Identity.RootID != "" {
		report.Connectivity = "connected"
	}
	if len(response.Envelope) > 0 {
		err = errors.Join(err, report.readStorageResponse(input.Action, response.Envelope))
	}
	return report, errors.Join(resultErr, err)
}

func (c Client) remoteStorageRequest(ctx context.Context, root string, intent remote.Intent, input StorageInput) (remote.Response, error) {
	configuration, err := loadSSHConfiguration(intent.SSHConfig)
	if err != nil {
		return remote.Response{}, err
	}
	coordinator, err := c.recordedRemoteCoordinator(configuration, root, intent)
	if err != nil {
		return remote.Response{}, err
	}
	if err = coordinator.RequireCapability(ctx, intent.Destination, remote.RetirementCapability); err != nil {
		return remote.Response{}, err
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		return remote.Response{}, err
	}
	operation := remote.Usage
	if input.Action == "retire" {
		operation = remote.Retire
	}
	return coordinator.Call(ctx, remote.Request{Version: remote.ProtocolVersion, Operation: operation, Root: intent.Destination.Root, Expected: &intent.Destination, TaskID: intent.TaskID, IntentSHA256: digest, Apply: input.Apply})
}

func (input StorageInput) validate() error {
	if (input.Action != "usage" && input.Action != "retire") || (input.Scope != "origin" && input.Scope != "destination" && input.Scope != "both") || (input.Action == "usage" && input.Apply) {
		return remote.ErrProtocol
	}
	return nil
}

func (c Client) storageOrigin(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, input StorageInput, report *Report) error {
	if input.Action == "usage" {
		local, err := payload.Usage(ctx, reader, id, remotedata.Origin)
		report.Usage["origin"] = local
		return err
	}
	local, err := c.retireLocal(ctx, reader, id, input.Apply)
	report.Retirement["origin"] = local
	return err
}

func (report *Report) readStorageResponse(action string, data json.RawMessage) error {
	if action == "usage" {
		var value payload.UsageReport
		if err := task.DecodeStrict(data, &value); err != nil {
			return err
		}
		report.Usage["destination"] = value
		return nil
	}
	var value payload.RetirementReport
	if err := task.DecodeStrict(data, &value); err != nil {
		return err
	}
	report.Retirement["destination"] = value
	return nil
}

func (c Client) retireLocal(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, apply bool) (value payload.RetirementReport, resultErr error) {
	if !apply {
		return payload.PreviewRetirement(ctx, reader, id, remotedata.Origin)
	}
	store, err := c.Stores.Open(ctx, reader.Root(), false, nil)
	if err != nil {
		return value, err
	}
	defer func() {
		if !taskdir.CleanupRetainsStore(resultErr, store) {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	return payload.Retire(ctx, store, id, remotedata.Origin, true)
}

func (c Client) storageRemote(ctx context.Context, request remote.Request, response remote.Response) (result remote.Response) {
	reader, err := c.Stores.Read(ctx, request.Root, c.pin)
	if err != nil {
		return storageFailure(response, err)
	}
	var operationErr error
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			result = storageFailure(result, errors.Join(operationErr, closeErr))
		}
	}()
	if request.Operation == remote.Usage {
		value, readErr := payload.Usage(ctx, reader, request.TaskID, remotedata.Destination)
		operationErr = readErr
		return storageEnvelope(response, value, readErr)
	}
	if !request.Apply {
		value, readErr := payload.PreviewRetirement(ctx, reader, request.TaskID, remotedata.Destination)
		operationErr = readErr
		return storageEnvelope(response, value, readErr)
	}
	store, err := c.Stores.Open(ctx, request.Root, false, c.pin)
	if err != nil {
		operationErr = err
		return storageFailure(response, err)
	}
	value, err := payload.Retire(ctx, store, request.TaskID, remotedata.Destination, true)
	if !taskdir.CleanupRetainsStore(err, store) {
		err = errors.Join(err, store.Close())
	}
	operationErr = err
	return storageEnvelope(response, value, err)
}

func storageEnvelope(response remote.Response, value any, cause error) remote.Response {
	data, err := json.Marshal(value)
	if err == nil && len(data) <= remote.MaxControlBytes/2 {
		response.Envelope = data
	} else {
		cause = errors.Join(cause, err, fmt.Errorf("remote storage report exceeds limit"))
	}
	if cause != nil {
		return storageFailure(response, cause)
	}
	return response
}

func storageFailure(response remote.Response, cause error) remote.Response {
	code := "storage_failed"
	if task.AllErrorCauses(cause, func(err error) bool { return remote.Transient(err) || errors.Is(err, task.ErrLockBusy) }) {
		code = "storage_busy"
	}
	response.Failure = remote.FailureResponse(code, cause)
	return response
}
