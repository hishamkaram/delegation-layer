package remotecommands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type LogInput struct {
	Stream    string
	Cursor    string
	Offset    int64
	Limit     int64
	OffsetSet bool
}

func readRemoteIntent(ctx context.Context, reader *taskdir.ReadOnlyStore, id string) (remote.Intent, error) {
	var intent remote.Intent
	data, err := reader.ReadRemoteIntentBytes(ctx, id)
	if err != nil {
		return intent, err
	}
	if err = task.DecodeStrict(data, &intent); err != nil {
		return intent, err
	}
	if err = intent.Validate(); err != nil {
		return intent, err
	}
	if intent.OriginRootID != reader.RootID() || intent.TaskID != id {
		return intent, remote.ErrIdentity
	}
	return intent, nil
}

func (c Client) remoteLogs(ctx context.Context, root, handle string, input LogInput) (report Report, resultErr error) {
	report.TaskID, report.Connectivity = handle, "not_connected"
	id, err := remote.ParseHandle(handle)
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
	intent, err := readRemoteIntent(ctx, reader, id)
	if err != nil {
		return report, err
	}
	configuration, err := loadSSHConfiguration(intent.SSHConfig)
	if err != nil {
		return report, err
	}
	coordinator, err := c.recordedRemoteCoordinator(configuration, root, intent)
	if err != nil {
		return report, err
	}
	if err = coordinator.RequireCapability(ctx, intent.Destination, remote.LogsCapability); err != nil {
		return report, err
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		return report, err
	}
	transfer, err := input.transfer()
	if err != nil {
		return report, err
	}
	response, err := coordinator.Call(ctx, remote.Request{Version: remote.ProtocolVersion, Operation: remote.Download, Expected: &intent.Destination, Root: intent.Destination.Root, TaskID: id, IntentSHA256: digest, Transfer: &transfer})
	report.Remote = &response
	if err != nil {
		return report, err
	}
	report.Connectivity = "connected"
	logs, err := logResponse(response, transfer, intent)
	if err != nil {
		return report, err
	}
	report.Logs = &logs
	return report, nil
}

func (input LogInput) transfer() (remote.TransferRequest, error) {
	transfer := remote.TransferRequest{Selector: "log:descriptors"}
	if input.Stream == "" {
		return transfer, nil
	}
	if input.Stream != "stdout" && input.Stream != "stderr" {
		return transfer, remote.ErrProtocol
	}
	if input.Limit == 0 {
		input.Limit = remote.MaxDataBytes
	}
	if input.Cursor != "" {
		offset, cursorErr := taskdir.LogCursorOffset(input.Cursor)
		if cursorErr != nil {
			return transfer, cursorErr
		}
		if input.OffsetSet && input.Offset != offset {
			return transfer, taskdir.ErrLogCursor
		}
		input.Offset = offset
	}
	if input.Offset < 0 || input.Limit < 1 || input.Limit > remote.MaxDataBytes {
		return transfer, remote.ErrProtocol
	}
	transfer = remote.TransferRequest{Selector: "log:" + input.Stream, Offset: input.Offset, Length: input.Limit, Cursor: input.Cursor}
	return transfer, nil
}

func logResponse(response remote.Response, transfer remote.TransferRequest, intent remote.Intent) (remote.LogReport, error) {
	var logs remote.LogReport
	if transfer.Selector == "log:descriptors" {
		err := task.DecodeStrict(response.Envelope, &logs)
		return logs, err
	}
	if response.Transfer == nil || response.Transfer.Range == nil {
		return logs, remote.ErrProtocol
	}
	rangeInfo := response.Transfer.Range
	stream := strings.TrimPrefix(transfer.Selector, "log:")
	if rangeInfo.Stream != stream || rangeInfo.Offset != transfer.Offset || rangeInfo.Length != int64(len(response.Data)) || rangeInfo.Length > transfer.Length || (len(response.Data) > 0 && rangeInfo.RangeSHA256 != task.ComputeSHA256(response.Data)) {
		return logs, remote.ErrProtocol
	}
	if err := taskdir.ValidateLogCursor(rangeInfo.Cursor, intent.Destination.RootID, intent.TaskID, stream, transfer.Offset+rangeInfo.Length); err != nil {
		return logs, err
	}
	data := base64.StdEncoding.EncodeToString(response.Data)
	logs.Range, logs.DataBase64 = rangeInfo, &data
	return logs, nil
}

func remoteLogDownload(ctx context.Context, reader *taskdir.ReadOnlyStore, request remote.Request, response remote.Response) remote.Response {
	if request.Transfer == nil {
		return logFailure(response, remote.ErrProtocol)
	}
	stored, err := reader.ReadTaskRequest(ctx, request.TaskID)
	if err != nil {
		return logFailure(response, err)
	}
	if stored.Remote == nil || request.IntentSHA256 == "" || stored.Remote.IntentSHA256 != request.IntentSHA256 {
		return logFailure(response, remote.ErrIdentity)
	}
	selector := strings.TrimPrefix(request.Transfer.Selector, "log:")
	if selector == "descriptors" {
		return remoteLogDescriptors(ctx, reader, request, response)
	}
	observed, err := reader.ReadTaskLogRange(ctx, request.TaskID, selector, request.Transfer.Offset, request.Transfer.Length, request.Transfer.Cursor)
	if err != nil {
		return logFailure(response, err)
	}
	rangeInfo := remote.LogRange{Stream: observed.Stream, Offset: observed.Offset, Length: observed.Length, ObservedSize: observed.ObservedSize, Sealed: observed.Sealed, WholeSHA256: observed.WholeSHA256, RangeSHA256: observed.RangeSHA256, Cursor: observed.Cursor}
	response.Data = observed.Data
	response.Transfer = &remote.TransferReceipt{Availability: "available", Length: observed.Length, Offset: observed.Offset + observed.Length, Range: &rangeInfo, Complete: observed.Sealed && observed.Offset+observed.Length == observed.ObservedSize}
	return response
}

func remoteLogDescriptors(ctx context.Context, reader *taskdir.ReadOnlyStore, request remote.Request, response remote.Response) remote.Response {
	if request.Transfer.Offset != 0 || request.Transfer.Length != 0 || request.Transfer.Cursor != "" {
		return logFailure(response, remote.ErrProtocol)
	}
	descriptors, readErr := reader.ReadTaskLogDescriptors(ctx, request.TaskID)
	if readErr != nil {
		return logFailure(response, readErr)
	}
	logs := remote.LogReport{}
	for _, d := range descriptors {
		name := filepath.Base(d.Path)
		if name != "stdout" && name != "stderr" {
			continue
		}
		logs.Streams = append(logs.Streams, remote.LogStream{Stream: name, Available: d.Available, Sealed: d.Sealed, Size: d.Size, SHA256: d.SHA256})
	}
	data, encodeErr := json.Marshal(logs)
	if encodeErr != nil {
		return logFailure(response, encodeErr)
	}
	response.Envelope = data
	return response
}

func logFailure(response remote.Response, cause error) remote.Response {
	code := "log_read_failed"
	switch {
	case errors.Is(cause, taskdir.ErrLogCursor):
		code = "log_cursor_invalid"
	case errors.Is(cause, taskdir.ErrLogObjectReplaced):
		code = "log_object_replaced"
	case errors.Is(cause, taskdir.ErrLogTruncated):
		code = "log_truncated"
	}
	response.Failure = remote.FailureResponse(code, cause)
	return response
}
