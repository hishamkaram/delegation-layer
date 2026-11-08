package remotecommands

import (
	"context"
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remote/payload"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func (c Client) Serve(input io.Reader, output io.Writer) int {
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
	response := c.Execute(ctx, request)
	if err = frame.WriteResponse(response); err != nil {
		return 1
	}
	return 0
}

func (c Client) Execute(ctx context.Context, request remote.Request) (response remote.Response) {
	response.Version = remote.ProtocolVersion
	if request.Operation == remote.Admit {
		response.TaskRecord = "not_created"
	}
	if request.Operation == remote.Reconcile {
		response.TaskRecord = "unknown"
	}
	if err := request.Validate(); err != nil {
		response.Failure = remote.FailureResponse("invalid_request", err)
		return response
	}
	root, err := tasklifecycle.ResolveRoot(request.Root)
	if err != nil {
		response.Failure = remote.FailureResponse("root_unavailable", err)
		return response
	}
	response.Identity.Root = root
	if request.Expected != nil && request.Root != request.Expected.Root {
		response.Failure = remote.FailureResponse("identity_mismatch", remote.ErrIdentity)
		return response
	}
	if err = c.prepareRemoteRoot(ctx, request.Operation, root); err != nil {
		response.Failure = remote.FailureResponse("root_unavailable", err)
		return response
	}
	reader, err := c.Stores.Read(ctx, root, c.pin)
	if err != nil {
		response.Failure = remote.FailureResponse("root_unavailable", err)
		return response
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			response.Diagnostics = append(response.Diagnostics, "remote root reader close failed: "+closeErr.Error())
		}
	}()
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		response.Failure = remote.FailureResponse("root_unavailable", err)
		return response
	}
	// Retain the original rooted reader until the operation settles. Every actual
	// mutable store opener also checks this same pin before granting a handle.
	defer func() {
		response = verifyRemoteFinalPin(ctx, reader, pin, request.Operation, response)
	}()
	if err = bindRemoteResponseIdentity(request, pin, &response); err != nil {
		response.Failure = remote.FailureResponse("identity_mismatch", err)
		return response
	}
	c.pin = &pin
	response.Capabilities = []string{"remote-v1", "existing-only-reconcile", "five-attempt-recovery", "declared-files", remote.ResourceGuardCapability}
	request.Root = response.Identity.Root
	if err = verifyRemoteOperationBinding(ctx, reader, request); err != nil {
		response.Failure = remoteBindingFailure(err)
		return response
	}

	return c.routeRemoteOperation(ctx, request, response)
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

func (c Client) prepareRemoteRoot(ctx context.Context, operation remote.Operation, root string) error {
	if operation != remote.Prepare {
		return nil
	}
	store, err := c.Stores.Open(ctx, root, true, c.pin)
	if err != nil {
		return err
	}
	return store.Close()
}

// bindRemoteResponseIdentity derives the reply identity from the selected root
// pin, retaining the caller's authenticated host and accepted root identity.
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

func (c Client) routeRemoteOperation(ctx context.Context, request remote.Request, response remote.Response) remote.Response {
	if request.Operation == remote.Hello || request.Operation == remote.Prepare {
		return c.probeRemoteContext(ctx, request, response)
	}
	if request.Expected == nil {
		response.Failure = remote.FailureResponse("identity_required", remote.ErrIdentity)
		return response
	}
	switch request.Operation {
	case remote.Status, remote.Collect, remote.Events:
		if response.RootPinChanged {
			return c.Local.Observe(ctx, *c.pin, request, response, true)
		}
		return c.Local.Observe(ctx, *c.pin, request, response, false)
	case remote.Download:
		return c.downloadRemote(ctx, request, response)
	case remote.Admit, remote.Reconcile:
		if !response.RootPinChanged {
			return c.admitRemote(ctx, request, response)
		}
	case remote.Cancel, remote.Reply, remote.Steer:
		if !response.RootPinChanged {
			return c.Local.Observe(ctx, *c.pin, request, response, false)
		}
	case remote.StageBegin, remote.StageChunk, remote.StageCommit:
		if !response.RootPinChanged {
			return c.transferRemote(ctx, request, response)
		}
	case remote.Hello, remote.Prepare:
		response.Failure = remote.FailureResponse("unsupported_operation", remote.ErrProtocol)
		return response
	}
	response.Failure = remote.FailureResponse("root_pin_changed", remote.ErrIdentity)
	return response
}

func (c Client) verifyRemoteAdmissionInputs(ctx context.Context, intent remote.Intent, digest string) error {
	if intent.InputSHA256 == "" {
		return nil
	}
	store, err := c.Stores.Open(ctx, intent.Destination.Root, false, c.pin)
	if err != nil {
		return err
	}
	inputErr := payload.VerifyInputs(ctx, store, intent.TaskID, digest, intent.InputSHA256, intent.RemoteCwd)
	return errors.Join(inputErr, store.Close())
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
	return remote.FailureResponse(code, err)
}

// A changed destination binding invalidates even an earlier absence receipt.
// Admission or recovery may already have progressed against the original root;
// the caller must reconcile the same identity rather than treat it as failed.
func remoteFinalPinFailure(operation remote.Operation, response remote.Response, cause error) remote.Response {
	failed := remote.Response{Version: remote.ProtocolVersion, Identity: response.Identity, Failure: remote.FailureResponse("root_identity_changed", cause), Diagnostics: response.Diagnostics}
	if operation == remote.Admit || operation == remote.Reconcile {
		failed.TaskRecord = "unknown"
		failed.Failure = remote.FailureResponse("acceptance_unknown", errors.Join(remote.ErrAcceptanceUnknown, cause))
	}
	return failed
}

func (c Client) probeRemoteContext(ctx context.Context, request remote.Request, response remote.Response) remote.Response {
	if err := c.requireRemoteResident(ctx, response.Identity.Root); err != nil {
		response.Failure = remote.FailureResponse("resident_not_ready", err)
		return response
	}
	if request.Provider != "" {
		return c.Local.Probe(ctx, *c.pin, request, response)
	}
	return response
}

func (c Client) admitRemote(ctx context.Context, request remote.Request, response remote.Response) remote.Response {
	if request.Intent == nil || request.Intent.TaskID != request.TaskID {
		response.Failure = remote.FailureResponse("invalid_intent", remote.ErrProtocol)
		return response
	}
	intent := *request.Intent
	if err := intent.Destination.Match(response.Identity); err != nil {
		response.Failure = remote.FailureResponse("identity_mismatch", err)
		return response
	}
	if int64(len(request.Data)) != intent.Brief.Length || task.ComputeSHA256(request.Data) != intent.Brief.SHA256 {
		response.Failure = remote.FailureResponse("brief_mismatch", remote.ErrConflict)
		return response
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		response.Failure = remote.FailureResponse("invalid_intent", err)
		return response
	}
	if request.Operation == remote.Admit {
		if err = c.requireRemoteResident(ctx, intent.Destination.Root); err != nil {
			response.Failure = remote.FailureResponse("resident_not_ready", err)
			return response
		}
	}
	if err = c.verifyRemoteAdmissionInputs(ctx, intent, digest); err != nil {
		response.Failure = remote.FailureResponse("input_unavailable", err)
		return response
	}
	binding := &task.RemoteBinding{OriginRootID: intent.OriginRootID, IntentSHA256: digest, InputSHA256: intent.InputSHA256, OutputPaths: intent.OutputPaths}
	return c.Local.Admit(ctx, *c.pin, request, binding, response)
}
