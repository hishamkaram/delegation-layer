package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func serveRemote(input io.Reader, output io.Writer, deps Dependencies) int {
	lifetime.PrepareRemoteControlProcess()
	ctx, cancel := context.WithTimeout(context.Background(), remote.DefaultTransferTimeout)
	defer cancel()
	reader, err := newRemoteInput(ctx, input, remote.DefaultTransferIdleTimeout)
	if err != nil {
		return 2
	}
	writer, err := newRemoteOutput(ctx, output, remote.DefaultTransferIdleTimeout)
	if err != nil {
		return 2
	}
	frame := remote.Framer{Reader: reader, Writer: writer}
	request, err := frame.ReadRequest()
	if err != nil {
		return 2
	}
	response := executeRemote(ctx, request, deps)
	if err = frame.WriteResponse(response); err != nil {
		return 1
	}
	return 0
}

func executeRemote(ctx context.Context, request remote.Request, deps Dependencies) (response remote.Response) {
	response.Version = remote.ProtocolVersion
	if request.Operation == remote.Admit {
		response.TaskRecord = "not_created"
	}
	if request.Operation == remote.Reconcile {
		response.TaskRecord = "unknown"
	}
	if err := request.Validate(); err != nil {
		response.Failure = remoteFailure("invalid_request", err)
		return response
	}
	root, err := tasklifecycle.ResolveRoot(request.Root)
	if err != nil {
		response.Failure = remoteFailure("root_unavailable", err)
		return response
	}
	response.Identity.Root = root
	if request.Expected != nil && request.Root != request.Expected.Root {
		response.Failure = remoteFailure("identity_mismatch", remote.ErrIdentity)
		return response
	}
	if err = prepareRemoteRoot(ctx, request.Operation, root, deps); err != nil {
		response.Failure = remoteFailure("root_unavailable", err)
		return response
	}
	reader, err := openPinnedRemoteReader(ctx, root, deps)
	if err != nil {
		response.Failure = remoteFailure("root_unavailable", err)
		return response
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			response.Diagnostics = append(response.Diagnostics, "remote root reader close failed: "+closeErr.Error())
		}
	}()
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		response.Failure = remoteFailure("root_unavailable", err)
		return response
	}
	// Retain the original rooted reader until the operation settles. Every actual
	// mutable store opener also checks this same pin before granting a handle.
	defer func() {
		response = verifyRemoteFinalPin(ctx, reader, pin, request.Operation, response)
	}()
	if err = bindRemoteResponseIdentity(request, pin, &response); err != nil {
		response.Failure = remoteFailure("identity_mismatch", err)
		return response
	}
	deps.remoteRootPin = &pin
	response.Capabilities = []string{"remote-v1", "existing-only-reconcile", "five-attempt-recovery", "declared-files"}
	request.Root = response.Identity.Root
	if err = verifyRemoteOperationBinding(ctx, reader, request); err != nil {
		response.Failure = remoteBindingFailure(err)
		return response
	}

	return routeRemoteOperation(ctx, request, response, deps)
}

func verifyRemoteFinalPin(ctx context.Context, reader *taskdir.ReadOnlyStore, pin taskdir.RootPin, operation remote.Operation, response remote.Response) remote.Response {
	observed, err := reader.PinRootContext(ctx)
	if err == nil && !observed.Equal(pin) {
		err = remote.ErrIdentity
	}
	if err != nil {
		return remoteFinalPinFailure(operation, response, err)
	}
	return response
}

func prepareRemoteRoot(ctx context.Context, operation remote.Operation, root string, deps Dependencies) error {
	if operation != remote.Prepare {
		return nil
	}
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), true)
	if err != nil {
		return err
	}
	return store.Close()
}

// openPinnedRemoteReader grants passive rooted reads only. It rejects a root
// changed since this operation selected its identity, without initializing it.
func openPinnedRemoteReader(ctx context.Context, root string, deps Dependencies) (*taskdir.ReadOnlyStore, error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return nil, err
	}
	if deps.remoteRootPin != nil {
		pin, pinErr := reader.PinRootContext(ctx)
		if pinErr == nil && !pin.Equal(*deps.remoteRootPin) {
			pinErr = remote.ErrIdentity
		}
		if pinErr != nil {
			return nil, errors.Join(pinErr, reader.Close())
		}
	}
	return reader, nil
}

func bindRemoteResponseIdentity(request remote.Request, pin taskdir.RootPin, response *remote.Response) error {
	response.Identity.RootID, response.Identity.RootDevice, response.Identity.RootInode = pin.RootID(), pin.Device(), pin.Inode()
	if request.Expected == nil {
		return nil
	}
	response.Identity.Host, response.Identity.Port, response.Identity.User, response.Identity.HostKey = request.Expected.Host, request.Expected.Port, request.Expected.User, request.Expected.HostKey
	if err := request.Expected.Match(response.Identity); err != nil {
		return err
	}
	response.RootPinChanged = request.Expected.PinChanged(response.Identity)
	return nil
}

func routeRemoteOperation(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	if request.Operation == remote.Hello || request.Operation == remote.Prepare {
		return probeRemoteContext(ctx, request, response, deps)
	}
	if request.Expected == nil {
		response.Failure = remoteFailure("identity_required", remote.ErrIdentity)
		return response
	}
	switch request.Operation {
	case remote.Status, remote.Collect, remote.Events:
		if response.RootPinChanged {
			return passiveRemote(ctx, request, response, deps)
		}
		return controlRemote(ctx, request, response, deps)
	case remote.Download:
		return downloadRemote(ctx, request, response, deps)
	case remote.Admit, remote.Reconcile:
		if !response.RootPinChanged {
			return admitRemote(ctx, request, response, deps)
		}
	case remote.Cancel, remote.Reply, remote.Steer:
		if !response.RootPinChanged {
			return controlRemote(ctx, request, response, deps)
		}
	case remote.StageBegin, remote.StageChunk, remote.StageCommit:
		if !response.RootPinChanged {
			return transferRemote(ctx, request, response, deps)
		}
	case remote.Hello, remote.Prepare:
		response.Failure = remoteFailure("unsupported_operation", remote.ErrProtocol)
		return response
	}
	response.Failure = remoteFailure("root_pin_changed", remote.ErrIdentity)
	return response
}

func probeRemoteContext(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	if err := requireRemoteResident(ctx, response.Identity.Root, deps); err != nil {
		response.Failure = remoteFailure("resident_not_ready", err)
		return response
	}
	if request.Provider != "" {
		return remoteCommandEnvelope(response, preflightContext(ctx, Arguments{Command: "preflight", Root: response.Identity.Root, Provider: request.Provider, Cwd: request.Cwd, Config: task.TaskConfig{Permission: "read-only"}}, deps))
	}
	return response
}

func remoteFailure(code string, err error) *remote.Failure {
	return &remote.Failure{Code: code, Message: err.Error()}
}

func admitRemote(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	if request.Intent == nil || request.Intent.TaskID != request.TaskID {
		response.Failure = remoteFailure("invalid_intent", remote.ErrProtocol)
		return response
	}
	intent := *request.Intent
	if err := intent.Destination.Match(response.Identity); err != nil {
		response.Failure = remoteFailure("identity_mismatch", err)
		return response
	}
	if int64(len(request.Data)) != intent.Brief.Length || task.ComputeSHA256(request.Data) != intent.Brief.SHA256 {
		response.Failure = remoteFailure("brief_mismatch", remote.ErrConflict)
		return response
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		response.Failure = remoteFailure("invalid_intent", err)
		return response
	}
	if request.Operation == remote.Admit {
		if err = requireRemoteResident(ctx, intent.Destination.Root, deps); err != nil {
			response.Failure = remoteFailure("resident_not_ready", err)
			return response
		}
	}
	if err = verifyRemoteAdmissionInputs(ctx, intent, digest, deps); err != nil {
		response.Failure = remoteFailure("input_unavailable", err)
		return response
	}
	binding := &task.RemoteBinding{OriginRootID: intent.OriginRootID, IntentSHA256: digest, InputSHA256: intent.InputSHA256, OutputPaths: intent.OutputPaths}
	a := Arguments{Command: "dispatch", Root: intent.Destination.Root, TaskID: intent.TaskID, Provider: intent.Provider, Cwd: intent.RemoteCwd, Config: intent.Config, BriefBytes: request.Data, remoteBinding: binding}
	var result commandResult
	if request.Operation == remote.Reconcile {
		result = dispatchCommandResult(tasklifecycle.ReconcileRemoteContext(ctx, dispatchRequest(a, deps), dispatchOptionsContext(ctx, deps, false)))
	} else {
		deps = deps.withCapturedEnvironmentContext(ctx)
		result = dispatchCommandResult(tasklifecycle.DispatchContext(ctx, dispatchRequest(a, deps), dispatchOptionsContext(ctx, deps, false)))
	}
	response = remoteCommandEnvelope(response, result)
	response.TaskRecord = result.response.TaskRecord
	if request.Operation == remote.Reconcile && response.TaskRecord == "not_created" {
		response.TaskRecord = "unknown"
	}
	if errors.Is(result.err, task.ErrRequestConflict) {
		response.Failure = remoteFailure("request_conflict", result.err)
	}
	if result.response.TaskRecord == "created" {
		response.Existing = true
		response.RequestSHA256 = digest
	}
	return response
}

func verifyRemoteAdmissionInputs(ctx context.Context, intent remote.Intent, digest string, deps Dependencies) error {
	if intent.InputSHA256 == "" {
		return nil
	}
	store, err := openStoreContext(ctx, intent.Destination.Root, deps.storeDependencies(), false)
	if err != nil {
		return err
	}
	inputErr := verifyRemoteInputs(ctx, store, intent.TaskID, digest, intent.InputSHA256, intent.RemoteCwd)
	return errors.Join(inputErr, store.Close())
}

func controlRemote(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	a := Arguments{Command: string(request.Operation), Root: request.Root, TaskID: request.TaskID, After: request.After, Watch: time.Duration(request.WatchNanos)}
	if request.Operation == remote.Cancel {
		if request.Control == nil {
			response.Failure = remoteFailure("invalid_control", remote.ErrProtocol)
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
	data, err := json.Marshal(result.response)
	if err != nil {
		response.Failure = remoteFailure("response_encoding", err)
		return response
	}
	if len(data) > remote.MaxControlBytes/2 {
		response.Failure = remoteFailure("response_too_large", ErrJSONResponseTooLarge)
		return response
	}
	response.Envelope = data
	return response
}

func remoteInteraction(ctx context.Context, request remote.Request, response remote.Response, deps Dependencies) remote.Response {
	if request.Control == nil || request.Control.Message == nil {
		response.Failure = remoteFailure("invalid_control", remote.ErrProtocol)
		return response
	}
	store, err := openStoreContext(ctx, request.Root, deps.storeDependencies(), false)
	if err != nil {
		response.Failure = remoteFailure("state_unavailable", err)
		return response
	}
	td, err := store.OpenTask(request.TaskID)
	if err != nil {
		response.Failure = remoteFailure("task_unavailable", errors.Join(err, store.Close()))
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
	reader, err := openPinnedRemoteReader(ctx, request.Root, deps)
	if err != nil {
		response.Failure = remoteFailure("state_unavailable", err)
		return response
	}
	envelope, readErr := passiveRemoteEnvelope(ctx, reader, request, response)
	readErr = errors.Join(readErr, reader.Close())
	if readErr != nil {
		response.Failure = remoteFailure("evidence_unavailable", readErr)
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

func remoteTaskOperation(operation remote.Operation) bool {
	switch operation {
	case remote.Status, remote.Collect, remote.Events, remote.Cancel, remote.Reply, remote.Steer, remote.Download:
		return true
	case remote.Hello, remote.Prepare, remote.Admit, remote.Reconcile, remote.StageBegin, remote.StageChunk, remote.StageCommit:
		return false
	}
	return false
}

func verifyRemoteTaskBinding(ctx context.Context, reader *taskdir.ReadOnlyStore, request remote.Request) error {
	stored, err := reader.ReadTaskRequest(ctx, request.TaskID)
	if task.RecordAbsent(err) {
		return remote.ErrAcceptanceUnknown
	}
	if err != nil {
		return err
	}
	if request.IntentSHA256 == "" || stored.Remote == nil || stored.Remote.IntentSHA256 != request.IntentSHA256 {
		return remote.ErrIdentity
	}
	return nil
}

func verifyRemoteOperationBinding(ctx context.Context, reader *taskdir.ReadOnlyStore, request remote.Request) error {
	if !remoteTaskOperation(request.Operation) {
		return nil
	}
	return verifyRemoteTaskBinding(ctx, reader, request)
}

func remoteBindingFailure(err error) *remote.Failure {
	code := "identity_mismatch"
	if errors.Is(err, remote.ErrAcceptanceUnknown) {
		code = "acceptance_unknown"
	}
	return remoteFailure(code, err)
}

// A changed destination binding invalidates even an earlier absence receipt.
// Admission or recovery may already have progressed against the original root;
// the caller must reconcile the same identity rather than treat it as failed.
func remoteFinalPinFailure(operation remote.Operation, response remote.Response, cause error) remote.Response {
	failed := remote.Response{Version: remote.ProtocolVersion, Identity: response.Identity, Failure: remoteFailure("root_identity_changed", cause), Diagnostics: response.Diagnostics}
	if operation == remote.Admit || operation == remote.Reconcile {
		failed.TaskRecord = "unknown"
		failed.Failure = remoteFailure("acceptance_unknown", errors.Join(remote.ErrAcceptanceUnknown, cause))
	}
	return failed
}
