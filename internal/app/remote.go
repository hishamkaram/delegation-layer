package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"

	"github.com/hishamkaram/delegation-layer/internal/app/remotecommands"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func runRemoteCLI(a Arguments, deps Dependencies, stdout, stderr io.Writer) int {
	client := remoteFeature(deps.normalized())
	if a.Command == "remote" && a.RemoteAction == "serve" {
		return client.Serve(deps.Input, stdout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), remote.DefaultTransferTimeout)
	defer cancel()
	report, err := remoteOrigin(ctx, a, client)
	if err != nil {
		report.Error = err.Error()
	}
	report.SchemaVersion, report.Command = OutputSchemaVersion, a.Command
	if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
		return writeCLIError(stderr, encodeErr, 1)
	}
	return remoteCLIResultCode(a.Command, report, err)
}

func remoteOrigin(ctx context.Context, a Arguments, client remotecommands.Client) (remotecommands.Report, error) {
	switch {
	case a.Command == "remote" && (a.RemoteAction == "usage" || a.RemoteAction == "retire"):
		return client.Storage(ctx, a.Root, remotecommands.StorageInput{Action: a.RemoteAction, TaskID: a.TaskID, Scope: a.RemoteScope, Apply: a.RemoteApply})
	case a.Command == "remote" && a.RemoteAction == "list":
		return client.List(ctx, a.Root, a.RemoteAfter)
	case a.Command == "devices":
		return client.Device(ctx, a.Root, remotecommands.DeviceInput{Action: a.RemoteAction, Device: a.Device, SSHConfig: a.SSHConfig, Fingerprint: a.Fingerprint, RemoteExecutable: a.RemoteExecutable, RemoteRoot: a.RemoteRoot, Provider: a.Provider, RemoteCwd: a.RemoteCwd})
	case a.Command == "dispatch":
		return client.Dispatch(ctx, a.Root, remotecommands.DispatchInput{TaskID: a.TaskID, Device: a.Device, SSHConfig: a.SSHConfig, RemoteExecutable: a.RemoteExecutable, RemoteRoot: a.RemoteRoot, RemoteCwd: a.RemoteCwd, Provider: a.Provider, Brief: a.Brief, BriefBytes: a.BriefBytes, InputManifest: a.InputManifest, OutputManifest: a.OutputManifest, Config: a.Config, ResourceGuards: a.ResourceGuards})
	default:
		var message *task.InteractionMessage
		var messageError error
		if a.Command == "reply" || a.Command == "steer" {
			value, err := interactionMessage(a)
			messageError = err
			message = &value
		}
		return client.Task(ctx, a.Root, remotecommands.TaskInput{Logs: remotecommands.LogInput{Stream: a.LogStream, Cursor: a.LogCursor, Offset: a.LogOffset, Limit: a.LogLimit, OffsetSet: a.LogOffsetSet}, Operation: a.Command, TaskID: a.TaskID, OutputDirectory: a.OutputDirectory, Watch: a.Watch, After: a.After, Control: message, ControlError: messageError})
	}
}

func remoteFeature(deps Dependencies) remotecommands.Client {
	adapter := remoteLocalAdapter{deps: deps}
	return remotecommands.Client{Stores: adapter, Local: adapter, Envelopes: remoteEnvelopeInspector{}}
}

type remoteLocalAdapter struct{ deps Dependencies }

func (a remoteLocalAdapter) Open(ctx context.Context, root string, create bool, pin *taskdir.RootPin) (*taskdir.Store, error) {
	deps := a.deps.storeDependencies()
	if pin != nil {
		deps.remoteRootPin = pin
	}
	return openStoreContext(ctx, root, deps, create)
}

func (a remoteLocalAdapter) Read(ctx context.Context, root string, expected *taskdir.RootPin) (*taskdir.ReadOnlyStore, error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, a.deps.storeDependencies().registry())
	if err != nil {
		return nil, err
	}
	if expected != nil {
		pin, pinErr := reader.PinRootContext(ctx)
		if pinErr == nil && !pin.Equal(*expected) {
			pinErr = remote.ErrIdentity
		}
		if pinErr != nil {
			return nil, errors.Join(pinErr, reader.Close())
		}
	}
	return reader, nil
}

func (a remoteLocalAdapter) Probe(ctx context.Context, pin taskdir.RootPin, request remote.Request, response remote.Response) remote.Response {
	deps := a.deps
	deps.remoteRootPin = &pin
	return remoteCommandEnvelope(response, preflightContext(ctx, Arguments{Command: "preflight", Root: response.Identity.Root, Provider: request.Provider, Cwd: request.Cwd, Config: task.TaskConfig{Permission: "read-only"}}, deps))
}

func (a remoteLocalAdapter) Admit(ctx context.Context, pin taskdir.RootPin, request remote.Request, binding *task.RemoteBinding, response remote.Response) remote.Response {
	deps := a.deps
	deps.remoteRootPin = &pin
	result := dispatchRemoteAdmission(ctx, request, binding, deps)
	if result.err != nil {
		retained, retired, retireErr := remoteFeature(deps).RetiredAdmission(ctx, pin, request, response, result.err)
		if retired && retireErr == nil {
			return retained
		}
		if retireErr != nil {
			result.err = errors.Join(result.err, retireErr)
		}
	}
	response = remoteCommandEnvelope(response, result)
	response.TaskRecord = result.response.TaskRecord
	if request.Operation == remote.Reconcile && response.TaskRecord == "not_created" {
		response.TaskRecord = "unknown"
	}
	if errors.Is(result.err, task.ErrRequestConflict) {
		response.Failure = remote.FailureResponse("request_conflict", result.err)
	}
	if result.response.TaskRecord == "created" {
		response.Existing = true
		response.RequestSHA256 = binding.IntentSHA256
	}
	return response
}

func (a remoteLocalAdapter) Observe(ctx context.Context, pin taskdir.RootPin, request remote.Request, response remote.Response, passive bool) remote.Response {
	deps := a.deps
	deps.remoteRootPin = &pin
	if passive {
		return passiveRemote(ctx, request, response, deps)
	}
	return controlRemote(ctx, request, response, deps)
}

type remoteEnvelopeInspector struct{}

func (remoteEnvelopeInspector) Inspect(data json.RawMessage) (remotecommands.ObservationFacts, error) {
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		return remotecommands.ObservationFacts{}, err
	}
	facts := remotecommands.ObservationFacts{Error: response.Error != "", Outcome: response.Outcome != nil, TimedOut: response.Status == "timed_out", Interrupted: response.Status == "interrupted", Events: len(response.Events) > 0, Pending: response.Pending != nil}
	if response.Interaction != nil {
		facts.InteractionClosed = response.Interaction.Closed
		facts.InteractionPending = len(response.Interaction.PendingRequests) > 0
	}
	return facts, nil
}

func remoteCLIResultCode(command string, report remotecommands.Report, err error) int {
	if err != nil {
		return remoteCommandErrorCode(command, report, err)
	}
	if report.Remote != nil && len(report.Remote.Envelope) > 0 {
		var envelope Response
		if decodeErr := json.Unmarshal(report.Remote.Envelope, &envelope); decodeErr != nil {
			return 1
		}
		if envelope.Error != "" {
			return 1
		}
		if envelope.Publication == "rejected" {
			return 4
		}
		if command == "collect" && envelope.Outcome == nil {
			return 3
		}
	}
	return 0
}

func remoteCommandErrorCode(command string, report remotecommands.Report, err error) int {
	if command == "logs" || report.Usage != nil || report.Retirement != nil {
		if task.AllErrorCauses(err, func(leaf error) bool { return remote.Transient(leaf) || errors.Is(leaf, task.ErrLockBusy) }) {
			return 3
		}
		return 1
	}
	if remote.Transient(err) || errors.Is(err, remote.ErrAcceptanceUnknown) {
		return 3
	}
	return 1
}

func controlRemote(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	a := Arguments{Command: string(request.Operation), Root: request.Root, TaskID: request.TaskID, After: request.After, Watch: time.Duration(request.WatchNanos)}
	if request.Operation == remote.Cancel {
		if request.Control == nil {
			response.Failure = remote.FailureResponse("invalid_control", remote.ErrProtocol)
			return response
		}
		a.remoteCancelID = request.Control.ID
	}
	if request.Operation == remote.Reply || request.Operation == remote.Steer {
		return remoteInteraction(ctx, request, response, deps)
	}
	return remoteCommandEnvelope(response, runCommandContext(ctx, a, deps))
}

func remoteCommandEnvelope(response remote.Response, result commandResult) remote.Response {
	var data bytes.Buffer
	err := writeJSONResponse(&data, result.response.Command, &result.response, result.err)
	if err != nil {
		code := "response_encoding"
		if errors.Is(err, ErrJSONResponseTooLarge) {
			code = "response_too_large"
		}
		response.Failure = remote.FailureResponse(code, err)
		return response
	}
	if data.Len() > remote.MaxControlBytes/2 {
		response.Failure = remote.FailureResponse("response_too_large", ErrJSONResponseTooLarge)
		return response
	}
	response.Envelope = data.Bytes()
	return response
}

func remoteInteraction(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	if request.Control == nil || request.Control.Message == nil {
		response.Failure = remote.FailureResponse("invalid_control", remote.ErrProtocol)
		return response
	}
	store, err := openStoreContext(ctx, request.Root, deps.storeDependencies(), false)
	if err != nil {
		response.Failure = remote.FailureResponse("state_unavailable", err)
		return response
	}
	td, err := store.OpenTask(request.TaskID)
	if err != nil {
		response.Failure = remote.FailureResponse("task_unavailable", errors.Join(err, store.Close()))
		return response
	}
	receipt, err := td.SubmitInteractionContext(ctx, *request.Control.Message)
	envelope := newResponse(string(request.Operation))
	envelope.RootID, envelope.TaskID = store.RootID, request.TaskID
	envelope.Delivery = &receipt
	err = errors.Join(err, td.Close(), store.Close())
	if err != nil {
		envelope.setError(err)
	}
	return remoteCommandEnvelope(response, commandResult{response: envelope, err: err})
}

func passiveRemote(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	reader, err := (remoteLocalAdapter{deps: deps}).Read(ctx, request.Root, deps.remoteRootPin)
	if err != nil {
		response.Failure = remote.FailureResponse("state_unavailable", err)
		return response
	}
	envelope, readErr := passiveRemoteEnvelope(ctx, reader, request, response)
	readErr = errors.Join(readErr, reader.Close())
	if readErr != nil {
		response.Failure = remote.FailureResponse("evidence_unavailable", readErr)
		return response
	}
	return remoteCommandEnvelope(response, commandResult{response: envelope})
}

func passiveRemoteEnvelope(ctx context.Context, reader *taskdir.ReadOnlyStore, request remote.Request, response remote.Response) (Response, error) {
	envelope := newResponse(string(request.Operation))
	saved, err := reader.ReadTaskRequest(ctx, request.TaskID)
	if err != nil {
		return envelope, err
	}
	if request.IntentSHA256 == "" || saved.Remote == nil || saved.Remote.IntentSHA256 != request.IntentSHA256 {
		return envelope, remote.ErrIdentity
	}
	evidence, err := reader.ReadTaskEvidence(ctx, request.TaskID)
	if err != nil {
		return envelope, err
	}
	if !evidence.Exists {
		return envelope, remote.ErrAcceptanceUnknown
	}
	envelope.RootID, envelope.TaskID = response.Identity.RootID, request.TaskID
	fillInspection(&envelope, evidence.Inspection)
	if request.Operation == remote.Events {
		state, events, cursor, readErr := reader.ReadTaskInteraction(ctx, request.TaskID, request.After)
		if readErr != nil {
			return envelope, readErr
		}
		envelope.Interaction, envelope.Events, envelope.Cursor = &state, events, &cursor
	}
	return envelope, nil
}

// Remote commands share the ordinary command implementations while retaining
// this dedicated control process's finite operation lifetime.

func runCommandContext(ctx context.Context, a Arguments, deps Dependencies) commandResult {
	switch a.Command {
	case "status":
		return statusContext(ctx, a, deps)
	case "collect":
		return collectContext(ctx, a, deps.storeDependencies())
	case "cancel":
		return cancelContext(ctx, a, deps)
	case "events", "reply", "steer":
		return interactionCommandContext(ctx, a, deps.storeDependencies())
	default:
		return failed(newResponse(a.Command), remote.ErrProtocol, 2)
	}
}

func dispatchRemoteAdmission(ctx context.Context, request remote.Request, binding *task.RemoteBinding, deps Dependencies) commandResult {
	intent := request.Intent
	a := Arguments{Command: "dispatch", Root: intent.Destination.Root, TaskID: intent.TaskID, Provider: intent.Provider, Cwd: intent.RemoteCwd, Config: intent.Config, BriefBytes: request.Data, remoteBinding: binding}
	if request.Operation == remote.Admit {
		deps = deps.withCapturedEnvironmentContext(ctx)
	}
	options := dispatchOptionsContext(ctx, deps, false)
	if request.ResourceGuards != nil {
		options.ResourceGuards = *request.ResourceGuards
	}
	var result commandResult
	if request.Operation == remote.Reconcile {
		result = dispatchCommandResult(tasklifecycle.ReconcileRemoteContext(ctx, dispatchRequest(a, deps), options))
	} else {
		result = dispatchCommandResult(tasklifecycle.DispatchContext(ctx, dispatchRequest(a, deps), options))
	}
	return result
}

func (a remoteLocalAdapter) Retired(ctx context.Context, pin taskdir.RootPin, request remote.Request, response remote.Response, cause error) remote.Response {
	reader, err := a.Read(ctx, request.Root, &pin)
	envelope := newResponse("dispatch")
	envelope.RootID, envelope.TaskID, envelope.TaskRecord = response.Identity.RootID, request.TaskID, "created"
	if err == nil {
		var readErr error
		envelope, readErr = passiveRemoteEnvelope(ctx, reader, request, response)
		err = errors.Join(readErr, reader.Close())
		envelope.Command = "dispatch"
		envelope.TaskRecord = "created"
	}
	if errors.Is(cause, config.ErrInvalidCwd) && task.AllErrorCauses(cause, func(leaf error) bool { return errors.Is(leaf, config.ErrInvalidCwd) || errors.Is(leaf, os.ErrNotExist) }) {
		cause = nil
	}
	return remoteCommandEnvelope(response, commandResult{response: envelope, err: errors.Join(cause, err)})
}
