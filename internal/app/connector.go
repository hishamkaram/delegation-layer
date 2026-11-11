package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/resultread"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// ConnectorResponse is additive public control data. Existing task lifecycle
// fields retain their independent authority; this envelope never replaces them.
type ConnectorResponse struct {
	ExecutionClosurePending taskdir.ConnectorClosurePendingReason `json:"execution_closure_pending,omitempty"`
	ExecutionClosure        *taskdir.ConnectorExecutionClosure    `json:"execution_closure,omitempty"`
	taskdir.ConnectorContextObservation
	OriginRegistered *bool                           `json:"origin_registered,omitempty"`
	Resolution       string                          `json:"resolution,omitempty"`
	Receipt          *taskdir.ConnectorReceipt       `json:"receipt,omitempty"`
	CreationAttempt  string                          `json:"creation_attempt,omitempty"`
	OriginBarrier    *ConnectorOriginBarrierResponse `json:"origin_barrier,omitempty"`
	Inventory        *taskdir.OriginInventory        `json:"inventory,omitempty"`
	ResultRange      *resultread.Range               `json:"result_range,omitempty"`
}

func readConnectorJSON(ctx context.Context, path string, input io.Reader, destination any) (resultErr error) {
	if path != "-" {
		file, err := openConnectorJSONFile(path)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		input = file
	}
	if input == nil {
		return errors.New("connector JSON stdin unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(connectorJSONReader{ctx: ctx, input: input}, task.MaxControlRecordSize+1))
	if err != nil {
		return err
	}
	return task.DecodeStrict(data, destination)
}

func openConnectorJSONFile(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("connector JSON path must be a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.Join(errors.New("connector JSON path changed or is not regular"), file.Close())
	}
	return file, nil
}

// The command borrows stdin exclusively; it never closes it, changes descriptor
// flags/deadlines, or starts a reader goroutine that could outlive cancellation.
type connectorJSONReader struct {
	ctx   context.Context
	input io.Reader
}

func (r connectorJSONReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	switch input := r.input.(type) {
	case *os.File:
		info, err := input.Stat()
		if err != nil {
			return 0, err
		}
		if info.Mode().IsRegular() {
			return input.Read(buffer)
		}
		if info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
			return 0, errors.New("connector JSON stdin must be a regular file, pipe or socket")
		}
		return r.readPollable(input, buffer)
	case *bytes.Reader, *bytes.Buffer, *strings.Reader:
		return r.input.Read(buffer)
	default:
		return 0, errors.New("connector JSON input requires a file or finite memory reader")
	}
}

func (r connectorJSONReader) readPollable(file *os.File, buffer []byte) (int, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var count int
	var readErr error
	controlErr := connection.Control(func(fd uintptr) {
		count, readErr = r.readPollDescriptor(fd, buffer)
	})
	return count, errors.Join(readErr, controlErr)
}

func (r connectorJSONReader) readPollDescriptor(fd uintptr, buffer []byte) (int, error) {
	if fd > 1<<31-1 {
		return 0, errors.New("connector JSON input descriptor outside poll range")
	}
	// The range check above makes the native poll descriptor conversion exact.
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		count, err := unix.Poll(poll, 25)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if count == 0 {
			continue
		}
		if poll[0].Revents&unix.POLLNVAL != 0 {
			return 0, errors.New("connector JSON input descriptor invalid")
		}
		n, readErr := unix.Read(int(fd), buffer)
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
			continue
		}
		if readErr != nil {
			return 0, readErr
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func loadConnectorOrigin(ctx context.Context, a *Arguments, deps Dependencies) error {
	if a.Connector.OriginFile == "" || a.Connector.origin != nil {
		return nil
	}
	var origin task.GatewayOrigin
	if err := readConnectorJSON(ctx, a.Connector.OriginFile, deps.Input, &origin); err != nil {
		return err
	}
	if err := task.ValidateGatewayOrigin(origin); err != nil {
		return err
	}
	a.Connector.origin = &origin
	return nil
}

func connectorFailure(response Response, err error) commandResult {
	code := 1
	if !taskdir.CleanupFault(err) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, task.ErrLockBusy) || errors.Is(cause, context.DeadlineExceeded)
	}) {
		code = 3
	}
	if errors.Is(err, taskdir.ErrOriginClosed) {
		if response.Connector == nil {
			response.Connector = &ConnectorResponse{}
		}
		response.Connector.CreationAttempt = "not_attempted"
	}
	result := failed(response, err, code)
	scrubConnectorFailure(&result)
	if errors.Is(err, context.DeadlineExceeded) && response.Command == "result" {
		result.response.Failure = &DispatchFailureResponse{Code: "result_verification_timeout", Stage: "verify-result", NextAction: "stop_and_report"}
		result.response.Error = "whole result verification exceeded its finite deadline; retrieve locally or explicitly configure compatible finite inner/client/HTTP deadlines"
	}
	return result
}

func connectorMutationRequested(a Arguments) bool {
	if a.Command == "context" {
		return a.Connector.Action == "enable-connector"
	}
	if a.Command == "origin" {
		return a.Connector.Action != "inventory"
	}
	return a.Command == "resolve" && (a.Connector.FenceIfAbsent || a.Connector.FenceUnattributedPartial || a.Connector.ResumeAdmission)
}

func connectorCommandContext(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse(a.Command)
	if err := loadConnectorOrigin(ctx, &a, deps); err != nil {
		return failed(response, err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	a.Root = root
	if connectorMutationRequested(a) {
		return connectorMutationContext(ctx, a, deps)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return connectorFailure(response, err)
	}
	defer func() { mergeConnectorClose(&result, reader.Close) }()
	response.RootID = reader.RootID()
	switch a.Command {
	case "context":
		return connectorContextInspection(ctx, a, reader, response)
	case "origin":
		return connectorInventoryInspection(ctx, a, reader, response)
	default:
		return connectorPassiveTaskContext(ctx, a, deps, reader, response)
	}
}

func connectorContextInspection(ctx context.Context, a Arguments, reader *taskdir.ReadOnlyStore, response Response) commandResult {
	observation, err := reader.InspectConnectorContext(ctx, a.Cwd)
	response.Connector = &ConnectorResponse{ConnectorContextObservation: observation}
	if err != nil {
		return connectorFailure(response, err)
	}
	if a.Connector.GatewayID != "" {
		registered := false
		response.Connector.OriginRegistered = &registered
		if slices.Contains(observation.Features, task.NamespaceAdmissionFeature) && slices.Contains(observation.Features, task.NamespaceMutationFeature) {
			state, readErr := reader.InspectOriginAdmissionContext(ctx, a.Connector.GatewayID, observation.RootToken)
			if readErr != nil {
				return connectorFailure(response, readErr)
			}
			registered = state != nil
			response.Connector.OriginBarrier = connectorOriginBarrierResponse(state)
		}
	}
	return commandResult{response: response}
}

func connectorInventoryInspection(ctx context.Context, a Arguments, reader *taskdir.ReadOnlyStore, response Response) commandResult {
	response.Connector = &ConnectorResponse{ConnectorContextObservation: taskdir.ConnectorContextObservation{ContractVersion: 1, RootToken: a.Connector.ExpectedRoot, Features: []string{}}}
	features, err := reader.ConnectorFeaturesContext(ctx)
	if err != nil {
		return connectorFailure(response, err)
	}
	response.Connector.Features = features
	inventory, err := reader.OriginInventoryContext(ctx, a.Connector.GatewayID, a.Connector.Generation, a.Connector.Cursor, a.Connector.Limit, a.Connector.ExpectedRoot)
	response.Connector.Inventory = &inventory
	if err != nil {
		return connectorFailure(response, err)
	}
	return commandResult{response: response}
}

func connectorPassiveTaskContext(ctx context.Context, a Arguments, deps Dependencies, reader *taskdir.ReadOnlyStore, response Response) commandResult {
	guard, err := reader.BorrowConnectorControlContext(ctx, a.Connector.ExpectedRoot, a.Connector.ExpectedProject)
	if err != nil {
		return connectorFailure(response, err)
	}
	response.Connector = &ConnectorResponse{ConnectorContextObservation: taskdir.ConnectorContextObservation{ContractVersion: 1, Features: []string{}, RootToken: a.Connector.ExpectedRoot, ProjectToken: a.Connector.ExpectedProject}}
	features, err := reader.ConnectorFeaturesContext(ctx)
	if err != nil {
		return connectorFailure(response, err)
	}
	response.Connector.Features = features
	response.TaskID = a.TaskID
	var result commandResult
	if a.Command == "result" {
		result = connectorResultContext(ctx, a, deps, reader, response)
	} else {
		result, err = resolveConnectorReaderContext(ctx, a, reader, response)
		if err != nil {
			result = connectorFailure(result.response, err)
		}
	}
	mergeConnectorClose(&result, func() error { return guard.Validate(ctx) })
	return result
}

func resolveConnectorReaderContext(ctx context.Context, a Arguments, reader *taskdir.ReadOnlyStore, response Response) (commandResult, error) {
	refusal, err := reader.ReadDurableNamespaceRefusalContext(ctx, a.TaskID)
	if err != nil {
		return commandResult{response: response}, err
	}
	if refusal != nil {
		if refusal.Origin != nil && !reflect.DeepEqual(refusal.Origin, a.Connector.origin) {
			return commandResult{response: response}, task.ErrRequestConflict
		}
		response.Connector.Resolution = "permanently_refused"
		return commandResult{response: response}, nil
	}
	exists, err := reader.ConnectorTaskEntryExistsContext(ctx, a.TaskID)
	if err != nil {
		return commandResult{response: response}, err
	}
	if !exists {
		response.Connector.Resolution = "absent"
		return commandResult{response: response}, nil
	}
	response.Connector.Resolution = "unresolved"
	receipt, err := reader.ReadConnectorReceiptContext(ctx, a.TaskID)
	if task.RecordAbsent(err) {
		return commandResult{response: response, code: 3}, nil
	}
	if err != nil {
		return commandResult{response: response}, err
	}
	if err = matchConnectorReceipt(a, *receipt); err != nil {
		return commandResult{response: response}, err
	}
	response.Connector.Receipt = receipt
	response.Connector.Resolution = "existing"
	facts, observation, err := tasklifecycle.ReadPassiveTaskObservation(ctx, reader, a.TaskID)
	if task.RecordAbsent(err) {
		response.Connector.Resolution = "unresolved"
		return commandResult{response: response, code: 3}, nil
	}
	if err != nil {
		return commandResult{response: response}, err
	}
	response.Admission, response.Liveness, response.Publication = observation.Admission.String(), observation.Liveness.String(), observation.Publication.String()
	if facts.Inspection != nil {
		response.Outcome = facts.Inspection.Outcome
	}
	closure, closureErr := reader.ReadConnectorExecutionClosureContext(ctx, a.TaskID)
	if closureErr != nil {
		return commandResult{response: response}, closureErr
	}
	response.Connector.ExecutionClosure = closure.Proof
	response.Connector.ExecutionClosurePending = closure.PendingReason
	return commandResult{response: response}, nil
}

func matchConnectorReceipt(a Arguments, receipt taskdir.ConnectorReceipt) error {
	if receipt.RootToken != a.Connector.ExpectedRoot || receipt.ProjectToken != a.Connector.ExpectedProject || a.Connector.origin == nil || receipt.Origin != *a.Connector.origin {
		return task.ErrRequestConflict
	}
	if a.Connector.ExpectedSpecSHA256 != "" && receipt.SpecSHA256 != a.Connector.ExpectedSpecSHA256 {
		return task.ErrRequestConflict
	}
	return nil
}

func connectorMutationContext(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse(a.Command)
	store, err := openStoreContext(ctx, a.Root, deps.storeDependencies(), false)
	if err != nil {
		return connectorFailure(response, err)
	}
	defer func() { closeObservationStore(&result, store) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	response.Connector = &ConnectorResponse{ConnectorContextObservation: taskdir.ConnectorContextObservation{ContractVersion: 1, Features: []string{}, RootToken: a.Connector.ExpectedRoot, ProjectToken: a.Connector.ExpectedProject}}
	features, err := store.ConnectorFeaturesContext(ctx)
	if err != nil {
		return connectorFailure(response, err)
	}
	response.Connector.Features = features
	if a.Command == "context" {
		if err = store.EnableConnectorContext(ctx, a.Connector.OfflineCutoverConfirmed); err != nil {
			return connectorFailure(response, err)
		}
		token, tokenErr := store.RootTokenContext(ctx)
		response.Connector.RootToken = token
		response.Connector.Features, err = store.ConnectorFeaturesContext(ctx)
		tokenErr = errors.Join(tokenErr, err)
		if tokenErr != nil {
			return connectorFailure(response, tokenErr)
		}
		return commandResult{response: response}
	}
	if a.Command == "origin" {
		return connectorOriginContext(ctx, a, store, response)
	}
	if a.Connector.ResumeAdmission {
		return connectorResumeGuardedContext(ctx, a, deps, store, response)
	}
	guard, err := store.BorrowConnectorControlContext(ctx, a.Connector.ExpectedRoot, a.Connector.ExpectedProject)
	if err != nil {
		return connectorFailure(response, err)
	}
	defer func() {
		mergeConnectorClose(&result, func() error { return guard.Validate(ctx) })
	}()
	var resolved taskdir.NamespaceResolution
	if a.Connector.FenceUnattributedPartial {
		resolved, err = store.FencePartialTaskContext(ctx, a.TaskID, taskdir.PartialFenceSelection{OperatorUnattributed: true, ExpectedSpecSHA256: a.Connector.ExpectedSpecSHA256})
	} else {
		resolved, err = store.FenceTaskNamespaceContext(ctx, a.TaskID, a.Connector.origin)
		if err == nil && resolved.Resolution == "existing" {
			resolved, err = fenceAttributedPartialContext(ctx, a, store)
		}
	}
	response.Connector.Resolution = resolved.Resolution
	if err != nil {
		return connectorFailure(response, err)
	}
	return commandResult{response: response}
}

func connectorResumeGuardedContext(ctx context.Context, a Arguments, deps Dependencies, store *taskdir.Store, response Response) (result commandResult) {
	guard, err := store.RetainConnectorContext(ctx, a.Connector.ExpectedRoot, a.Connector.ExpectedProject)
	if err != nil {
		return connectorFailure(response, err)
	}
	binding := guard.Binding()
	a.Connector.binding = &binding
	defer func() {
		mergeConnectorClose(&result, func() error { return errors.Join(guard.Validate(ctx), guard.Close()) })
	}()
	return connectorResumeContext(ctx, a, deps, store, response)
}

func connectorOriginContext(ctx context.Context, a Arguments, store *taskdir.Store, response Response) commandResult {
	c := a.Connector
	var err error
	switch c.Action {
	case "barrier":
		state, transitionErr := store.OriginBarrierContext(ctx, c.GatewayID, c.RecoveryID, c.ExpectedGeneration, c.Generation, c.ExpectedRoot)
		response.Connector.OriginBarrier, err = connectorOriginBarrierResponse(state), transitionErr
	case "inventory":
		inventory, readErr := store.OriginInventoryContext(ctx, c.GatewayID, c.Generation, c.Cursor, c.Limit, c.ExpectedRoot)
		response.Connector.Inventory, err = &inventory, readErr
	case "reopen":
		state, transitionErr := store.OriginReopenContext(ctx, c.GatewayID, c.RecoveryID, c.Generation, c.CompletenessReceipt, c.ExpectedRoot)
		response.Connector.OriginBarrier, err = connectorOriginBarrierResponse(state), transitionErr
	default:
		err = errors.New("unsupported origin operation")
	}
	if err != nil {
		return connectorFailure(response, err)
	}
	return commandResult{response: response}
}

func connectorResumeContext(ctx context.Context, a Arguments, deps Dependencies, store *taskdir.Store, response Response) (result commandResult) {
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return connectorFailure(response, err)
	}
	// Validate complete prepared bytes BEFORE entering the existing recovery owner;
	// ExistingOnly also forbids fallback if records disappear before its re-read.
	request, meta, spec, _, err := td.PreparedIdentity()
	if err != nil {
		return connectorFailure(response, errors.Join(err, td.Close()))
	}
	if request.Origin == nil || request.ConnectorContext == nil || *request.Origin != *a.Connector.origin || request.ConnectorContext.RootToken != a.Connector.ExpectedRoot || spec != a.Connector.ExpectedSpecSHA256 {
		return connectorFailure(response, errors.Join(task.ErrRequestConflict, td.Close()))
	}
	project, err := taskdir.ProjectToken(request.ConnectorContext.Project)
	if err != nil || project != a.Connector.ExpectedProject {
		return connectorFailure(response, errors.Join(task.ErrIdentityMismatch, err, td.Close()))
	}
	if err = td.RequireTaskLaunchPermissionContext(ctx); err != nil {
		return connectorFailure(response, errors.Join(err, td.Close()))
	}
	options := dispatchOptionsContext(ctx, deps, false)
	observed := tasklifecycle.DispatchExistingContext(ctx, tasklifecycle.ExistingTaskOptions{ExistingOnly: true, Creation: options.Creation, Selection: supervisorcore.AdmissionSelection{Runner: meta.RunnerExecutable, RunnerOwnership: meta.RunnerOwnership, ConfigPath: meta.SupervisorConfig.ConfigPath}, RequestedBudget: request.RequestedConfig.Budget}, store, td, *request)
	result = dispatchCommandResult(tasklifecycle.DispatchResult{Root: store.Root, RootID: store.RootID, TaskID: a.TaskID, Provider: request.Provider, Record: tasklifecycle.RecordCreated, Existing: &observed})
	result.response.Command = "resolve"
	result.response.Connector = response.Connector
	result = attachConnectorDispatchReceipt(ctx, a, deps, result)
	result.response.Connector.Resolution = "existing"
	return result
}

func connectorResultContext(ctx context.Context, a Arguments, deps Dependencies, reader *taskdir.ReadOnlyStore, response Response) commandResult {
	receipt, err := reader.ReadConnectorReceiptContext(ctx, a.TaskID)
	if err != nil {
		return connectorFailure(response, err)
	}
	if err = matchConnectorReceipt(a, *receipt); err != nil {
		return connectorFailure(response, err)
	}
	var expected task.PayloadDescriptor
	if err = readConnectorJSON(ctx, a.Connector.DescriptorFile, deps.Input, &expected); err != nil {
		return connectorFailure(response, err)
	}
	outcome, err := reader.ReadCommittedResultOutcomeContext(ctx, a.TaskID)
	if err != nil {
		return connectorFailure(response, err)
	}
	if outcome.Payload != expected {
		return connectorFailure(response, task.ErrRequestConflict)
	}

	file, err := reader.OpenTaskPayload(ctx, a.TaskID, expected)
	if err != nil {
		return connectorFailure(response, err)
	}
	observed, readErr := resultread.Read(ctx, file, expected, a.Connector.Offset, a.Connector.ResultLimit)
	if err = errors.Join(readErr, file.Close()); err != nil {
		return connectorFailure(response, err)
	}
	response.Connector.Receipt, response.Connector.ResultRange = receipt, &observed
	response.Publication = task.PublicationCommitted.String()
	response.Outcome = outcome
	return commandResult{response: response}
}

func retainOpenedTaskGuardContext(ctx context.Context, a Arguments, store *taskdir.Store, td *taskdir.TaskDir) (*taskdir.ConnectorControlGuard, error) {
	if a.Connector.ExpectedRoot == "" {
		return nil, nil
	}
	if a.Connector.origin == nil {
		return nil, errors.New("guarded task operation has no validated immutable origin")
	}
	request, _, spec, _, err := td.PreparedIdentity()
	if err != nil {
		return nil, err
	}
	if request.Origin == nil || request.ConnectorContext == nil {
		return nil, task.ErrRequestConflict
	}
	project, err := taskdir.ProjectToken(request.ConnectorContext.Project)
	if err != nil {
		return nil, err
	}
	if *request.Origin != *a.Connector.origin || project != a.Connector.ExpectedProject || request.ConnectorContext.RootToken != a.Connector.ExpectedRoot {
		return nil, task.ErrRequestConflict
	}
	if a.Connector.ExpectedSpecSHA256 != "" && spec != a.Connector.ExpectedSpecSHA256 {
		return nil, task.ErrRequestConflict
	}
	return store.BorrowConnectorControlContext(ctx, a.Connector.ExpectedRoot, a.Connector.ExpectedProject)
}

func prepareGuardedDispatchContext(ctx context.Context, a *Arguments, deps Dependencies) error {
	if a.Connector.ExpectedRoot == "" {
		return nil
	}
	if err := loadConnectorOrigin(ctx, a, deps); err != nil {
		return err
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return err
	}
	binding, readErr := reader.RequireConnectorContext(ctx, a.Connector.ExpectedRoot, a.Connector.ExpectedProject)
	if err = errors.Join(readErr, reader.Close()); err != nil {
		return err
	}
	requestedProject, projectErr := taskdir.ObserveProjectContext(ctx, a.Cwd)
	if projectErr != nil {
		return projectErr
	}
	if binding.Project != requestedProject {
		return task.ErrIdentityMismatch
	}
	a.Cwd = binding.Project.Path
	a.Root, a.Connector.binding = root, binding
	return nil
}

func attachConnectorDispatchReceipt(ctx context.Context, a Arguments, deps Dependencies, result commandResult) commandResult {
	if a.Connector.ExpectedRoot == "" {
		return result
	}
	result.response.Connector = &ConnectorResponse{ConnectorContextObservation: taskdir.ConnectorContextObservation{ContractVersion: 1, Features: []string{}, RootToken: a.Connector.ExpectedRoot, ProjectToken: a.Connector.ExpectedProject}}
	if taskdir.CreationNotAttempted(result.err) || errors.Is(result.err, taskdir.ErrOriginClosed) {
		result.response.Connector.CreationAttempt = "not_attempted"
		if !taskdir.CleanupFault(result.err) && task.AllErrorCauses(result.err, func(cause error) bool {
			return errors.Is(cause, task.ErrLockBusy) || errors.Is(cause, context.DeadlineExceeded)
		}) {
			result.code = 3
			result.response.Failure = &DispatchFailureResponse{Code: "creation_not_attempted", Stage: "task-namespace", NextAction: "retry_later"}
		}
	}
	if result.response.TaskRecord != "created" && result.code != 0 {
		return result
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, a.Root, deps.storeDependencies().registry())
	if err != nil {
		return connectorFailure(result.response, errors.Join(result.err, err))
	}
	features, featureErr := reader.ConnectorFeaturesContext(ctx)
	result.response.Connector.Features = features
	if featureErr != nil || result.response.TaskRecord != "created" {
		if err = errors.Join(featureErr, reader.Close()); err != nil {
			return connectorFailure(result.response, errors.Join(result.err, err))
		}
		return result
	}
	receipt, readErr := reader.ReadConnectorReceiptContext(ctx, a.TaskID)
	if err = errors.Join(readErr, reader.Close()); err != nil {
		return connectorFailure(result.response, errors.Join(result.err, err))
	}
	if err = matchConnectorReceipt(a, *receipt); err != nil {
		return connectorFailure(result.response, errors.Join(result.err, err))
	}
	result.response.Connector.Receipt = receipt
	result.response.Connector.Resolution = "existing"
	return result
}

func fenceAttributedPartialContext(ctx context.Context, a Arguments, store *taskdir.Store) (taskdir.NamespaceResolution, error) {
	existing := taskdir.NamespaceResolution{Resolution: "existing"}
	identity, err := store.RecordedTaskIdentityContext(ctx, a.TaskID)
	if task.RecordAbsent(err) {
		return taskdir.NamespaceResolution{Resolution: "unresolved"}, nil
	}
	if err != nil {
		return existing, err
	}
	receipt, err := identity.ConnectorReceipt()
	if err != nil {
		return existing, err
	}
	if err = matchConnectorReceipt(a, *receipt); err != nil {
		return existing, err
	}
	return store.FencePartialTaskContext(ctx, a.TaskID, taskdir.PartialFenceSelection{Frozen: identity, ExpectedSpecSHA256: a.Connector.ExpectedSpecSHA256})
}

type ConnectorOriginBarrierResponse struct {
	GatewayID     string `json:"gateway_id"`
	RecoveryID    string `json:"recovery_id"`
	Generation    string `json:"generation"`
	AdmissionOpen bool   `json:"admission_open"`
}

func connectorOriginBarrierResponse(state *taskdir.OriginAdmission) *ConnectorOriginBarrierResponse {
	if state == nil {
		return nil
	}
	return &ConnectorOriginBarrierResponse{GatewayID: state.GatewayID, RecoveryID: state.RecoveryID, Generation: state.Generation, AdmissionOpen: state.AdmissionOpen}
}

func mergeConnectorClose(result *commandResult, closeOperation func() error) {
	mergeCommandClose(result, closeOperation)
	scrubConnectorFailure(result)
}

// Every connector failure invalidates release/readiness proofs, including faults
// from final context validation or deferred cleanup after a mutation succeeded.
func scrubConnectorFailure(result *commandResult) {
	if result.err == nil && result.code == 0 {
		return
	}
	if result.response.Stop != nil {
		result.response.Stop.Terminated = false
	}
	connector := result.response.Connector
	if connector == nil {
		return
	}
	connector.Features = nil
	connector.RootToken = ""
	connector.ProjectToken = ""
	connector.ResultRange = nil
	connector.ExecutionClosure = nil
	connector.ExecutionClosurePending = ""
	connector.Inventory = nil
	connector.OriginBarrier = nil
	connector.OriginRegistered = nil
	if connector.Resolution == "permanently_refused" {
		connector.Resolution = "unresolved"
	}
}
