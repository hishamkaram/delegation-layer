package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app/remotecommands"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Workbench composes in-process consumers with the same command and lifecycle
// owners as the CLI. It owns no task state and does not render terminal output.
type Workbench struct{ deps Dependencies }

// OperationResult preserves the command's result/exit semantics without a
// serialize/parse bridge. RestartRequired retains, rather than closes, custody
// that the operation owner could not safely release.
type OperationResult struct {
	Response        Response
	Remote          *remotecommands.Report
	Code            int
	Err             error
	RestartRequired bool
}

// DispatchPreview preserves requested intent separately from resolved display
// values. Arguments pins the catalog/revision selected by the same resolver
// that admission uses, without granting admission authority.
type DispatchPreview struct {
	Arguments Arguments
	Provider  string
	Auto      bool
	Config    task.TaskConfig
	Team      *task.TeamContext
}

// ContentPreview retains bounded bytes only after the complete sealed payload
// has matched its canonical descriptor. It is never cached by the workbench.
type ContentPreview struct {
	Data       []byte
	TotalBytes int64
	Truncated  bool
}

func (w *Workbench) ReadPublishedResult(ctx context.Context, a Arguments, limit int64) (ContentPreview, error) {
	if limit == 0 {
		limit = taskdir.MaxLogRangeBytes
	}
	if limit < 1 || limit > taskdir.MaxLogRangeBytes {
		return ContentPreview{}, errors.New("result preview limit must be between 1 and 65536 bytes")
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return ContentPreview{}, err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, w.deps.storeDependencies().registry())
	if err != nil {
		return ContentPreview{}, err
	}
	if strings.HasPrefix(a.TaskID, "remote:") {
		return w.readRemoteResult(ctx, a, reader, limit)
	}
	facts, observation, readErr := tasklifecycle.ReadPassiveTaskObservation(ctx, reader, a.TaskID)
	if readErr != nil {
		return ContentPreview{}, errors.Join(readErr, reader.Close())
	}
	if observation.Publication != task.PublicationCommitted || facts.Inspection == nil || facts.Inspection.Outcome == nil {
		return ContentPreview{}, errors.Join(errors.New("committed result unavailable; inspect or collect the task first"), reader.Close())
	}
	descriptor := facts.Inspection.Outcome.Payload
	file, err := reader.OpenTaskPayload(ctx, a.TaskID, descriptor)
	if err != nil {
		return ContentPreview{}, errors.Join(err, reader.Close())
	}
	result, readErr := readPublishedPrefix(ctx, file, descriptor, limit)
	return result, errors.Join(readErr, file.Close(), reader.Close())
}

func (w *Workbench) readRemoteResult(ctx context.Context, a Arguments, reader *taskdir.ReadOnlyStore, limit int64) (ContentPreview, error) {
	id, err := remote.ParseHandle(a.TaskID)
	if err != nil {
		return ContentPreview{}, errors.Join(err, reader.Close())
	}
	var intent remote.Intent
	err = reader.ReadCanonicalRemoteRecord(ctx, id, true, false, "intent.json", &intent)
	if err != nil {
		return ContentPreview{}, errors.Join(err, reader.Close())
	}
	observation, readErr := remote.ReadLastObservation(ctx, reader, id, intent)
	if err = errors.Join(readErr, reader.Close()); err != nil {
		return ContentPreview{}, err
	}
	var response Response
	if err = json.Unmarshal(observation.Response.Envelope, &response); err != nil {
		return ContentPreview{}, err
	}
	if response.Publication != task.PublicationCommitted.String() || response.Outcome == nil || response.Outcome.Verdict != task.VerdictCommitted {
		return ContentPreview{}, errors.New("committed remote result unavailable in origin observation; inspect or collect the remote task first")
	}
	if response.RootID != intent.Destination.RootID || response.TaskID != id || response.Outcome.RootID != response.RootID || response.Outcome.TaskID != id {
		return ContentPreview{}, task.ErrIdentityMismatch
	}
	if err = task.ValidateOutcomeRecord(response.Outcome); err != nil {
		return ContentPreview{}, err
	}
	transferContext, cancel := context.WithTimeout(ctx, remote.DefaultTransferTimeout)
	defer cancel()
	file, err := remoteFeature(w.deps).PayloadReader(transferContext, a.Root, a.TaskID, response.Outcome.Payload)
	if err != nil {
		return ContentPreview{}, err
	}
	return readPublishedPrefix(transferContext, file, response.Outcome.Payload, limit)
}

func readPublishedPrefix(ctx context.Context, source io.Reader, descriptor task.PayloadDescriptor, limit int64) (ContentPreview, error) {
	if err := task.ValidatePayloadDescriptor(descriptor); err != nil {
		return ContentPreview{}, err
	}
	result := ContentPreview{TotalBytes: descriptor.Length, Truncated: descriptor.Length > limit}
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var length int64
	for {
		if err := ctx.Err(); err != nil {
			return ContentPreview{}, err
		}
		n, err := source.Read(buffer)
		if n > 0 {
			length += int64(n)
			if length > descriptor.Length {
				return ContentPreview{}, task.ErrEvidenceFault
			}
			if _, writeErr := hash.Write(buffer[:n]); writeErr != nil {
				return ContentPreview{}, writeErr
			}
			keep := min(int64(n), max(0, limit-int64(len(result.Data))))
			result.Data = append(result.Data, buffer[:keep]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ContentPreview{}, err
		}
		if n == 0 {
			return ContentPreview{}, io.ErrNoProgress
		}
	}
	if length != descriptor.Length || hex.EncodeToString(hash.Sum(nil)) != descriptor.SHA256 {
		return ContentPreview{}, task.ErrEvidenceFault
	}
	return result, nil
}

func (w *Workbench) ReadLogRange(ctx context.Context, a Arguments) (taskdir.LogRange, error) {
	if strings.HasPrefix(a.TaskID, "remote:") {
		return w.readRemoteLogRange(ctx, a)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return taskdir.LogRange{}, err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, w.deps.storeDependencies().registry())
	if err != nil {
		return taskdir.LogRange{}, err
	}
	limit := a.LogLimit
	if limit == 0 {
		limit = taskdir.MaxLogRangeBytes
	}
	if a.LogCursor != "" {
		offset, cursorErr := taskdir.LogCursorOffset(a.LogCursor)
		if cursorErr != nil {
			return taskdir.LogRange{}, errors.Join(cursorErr, reader.Close())
		}
		if a.LogOffsetSet && a.LogOffset != offset {
			return taskdir.LogRange{}, errors.Join(taskdir.ErrLogCursor, reader.Close())
		}
		a.LogOffset = offset
	}
	observed, readErr := reader.ReadTaskLogRange(ctx, a.TaskID, a.LogStream, a.LogOffset, limit, a.LogCursor)
	return observed, errors.Join(readErr, reader.Close())
}

func (w *Workbench) readRemoteLogRange(ctx context.Context, a Arguments) (taskdir.LogRange, error) {
	report, err := remoteFeature(w.deps).Task(ctx, a.Root, remotecommands.TaskInput{Operation: "logs", TaskID: a.TaskID, Logs: remotecommands.LogInput{Stream: a.LogStream, Offset: a.LogOffset, Limit: a.LogLimit, Cursor: a.LogCursor, OffsetSet: a.LogOffsetSet}})
	if err != nil {
		return taskdir.LogRange{}, err
	}
	if report.Logs == nil || report.Logs.Range == nil || report.Logs.DataBase64 == nil {
		return taskdir.LogRange{}, remote.ErrProtocol
	}
	data, err := base64.StdEncoding.DecodeString(*report.Logs.DataBase64)
	if err != nil {
		return taskdir.LogRange{}, err
	}
	info := report.Logs.Range
	return taskdir.LogRange{Stream: info.Stream, Offset: info.Offset, Length: info.Length, ObservedSize: info.ObservedSize, Sealed: info.Sealed, WholeSHA256: info.WholeSHA256, RangeSHA256: info.RangeSHA256, Cursor: info.Cursor, Data: data}, nil
}

func (w *Workbench) PreviewDispatch(ctx context.Context, a Arguments) (DispatchPreview, error) {
	a.Command = "dispatch"
	if err := a.validate(nil, a.Watch.String()); err != nil {
		return DispatchPreview{}, err
	}
	preview := DispatchPreview{Arguments: a, Provider: a.Provider, Auto: a.Auto, Config: a.Config}
	target := requestedTeamTarget(a)
	if target == nil && a.ExpectedTeamSelection != nil {
		return preview, teamcontract.ErrTeamSelectionChanged
	}
	if a.Device != "" {
		return preview, nil
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return preview, err
	}
	cwd, err := w.previewTeamCwd(ctx, a, root)
	if err != nil {
		return preview, err
	}
	if target == nil {
		return preview, nil
	}
	member, err := lookupTeamMember(ctx, *target, root, cwd, a.ProjectRoot, w.deps)
	if err != nil {
		return preview, err
	}
	intent := task.TeamIntent{Provider: a.Provider, Auto: a.Auto, Config: a.Config}
	selected, err := teamcontract.Select(member, intent)
	if err != nil {
		return preview, err
	}
	preview.Provider, preview.Auto, preview.Config, preview.Team = selected.Execution.Provider, selected.Execution.Auto, selected.Execution.Config, selected.Context
	pinned := selected.Context.Target.Clone()
	preview.Arguments.teamTarget = &pinned
	expected := selected.Clone()
	preview.Arguments.ExpectedTeamSelection = &expected
	return preview, nil
}

func (w *Workbench) previewTeamCwd(ctx context.Context, a Arguments, root string) (string, error) {
	if a.WorkspaceID == "" {
		_, cwd, err := config.ValidateDirectories(root, a.Cwd)
		return cwd, err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, w.deps.storeDependencies().registry())
	if err != nil {
		return "", err
	}
	request, readErr := workspace.ReadResourceRequest(ctx, reader, a.WorkspaceID)
	return request.SourceCwd, errors.Join(readErr, reader.Close())
}

func NewWorkbench(deps Dependencies) *Workbench {
	return &Workbench{deps: deps.withCapturedEnvironment().normalized()}
}

func workbenchResult(result commandResult) OperationResult {
	retained := task.AnyErrorNode(result.err, func(node error) bool {
		var admission *taskAdmissionCleanupError
		if errors.As(node, &admission) && admission.store != nil {
			return true
		}
		var owner interface{ RetainedStore() *taskdir.Store }
		if errors.As(node, &owner) && owner.RetainedStore() != nil {
			return true
		}
		var lifetime *taskdir.LifetimeOwnerCleanupError
		return errors.As(node, &lifetime) && lifetime != nil && lifetime.Lease() != nil && lifetime.Lease().CleanupPending()
	})
	return OperationResult{Response: result.response, Code: result.code, Err: result.err, RestartRequired: retained}
}

func workbenchInputFailure(command string, err error) OperationResult {
	return workbenchResult(failed(newResponse(command), err, 2))
}

func (w *Workbench) Dispatch(ctx context.Context, a Arguments) OperationResult {
	a.Command = "dispatch"
	if err := a.validate(nil, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	if isRemoteCommand(a) {
		return w.remote(ctx, a)
	}
	return workbenchResult(dispatchContext(ctx, a, w.deps))
}

func (w *Workbench) Continue(ctx context.Context, a Arguments) OperationResult {
	a.Command = "continue"
	if err := a.validate(nil, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(continueTaskContext(ctx, a, w.deps))
}

func (w *Workbench) Inspect(ctx context.Context, a Arguments) OperationResult {
	return w.taskOperation(ctx, a, "status")
}

func (w *Workbench) Collect(ctx context.Context, a Arguments) OperationResult {
	return w.taskOperation(ctx, a, "collect")
}

func (w *Workbench) Cancel(ctx context.Context, a Arguments) OperationResult {
	return w.taskOperation(ctx, a, "cancel")
}

func (w *Workbench) Recheck(a Arguments) OperationResult {
	a.Command = "recheck"
	if err := a.validate([]string{a.TaskID}, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(recheck(a, w.deps.storeDependencies()))
}

func (w *Workbench) Events(ctx context.Context, a Arguments) OperationResult {
	return w.taskOperation(ctx, a, "events")
}

func (w *Workbench) Reply(ctx context.Context, a Arguments) OperationResult {
	return w.taskOperation(ctx, a, "reply")
}

func (w *Workbench) Steer(ctx context.Context, a Arguments) OperationResult {
	return w.taskOperation(ctx, a, "steer")
}

func (w *Workbench) Logs(ctx context.Context, a Arguments) OperationResult {
	return w.taskOperation(ctx, a, "logs")
}

func (w *Workbench) taskOperation(ctx context.Context, a Arguments, command string) OperationResult {
	a.Command = command
	if err := a.validate([]string{a.TaskID}, a.Watch.String()); err != nil {
		return workbenchInputFailure(command, err)
	}
	if isRemoteCommand(a) {
		return w.remote(ctx, a)
	}
	switch command {
	case "logs":
		return workbenchResult(logsContext(ctx, a, w.deps.storeDependencies()))
	default:
		return workbenchResult(runCommandContext(ctx, a, w.deps))
	}
}

func (w *Workbench) remote(ctx context.Context, a Arguments) OperationResult {
	ctx, cancel := context.WithTimeout(ctx, remote.DefaultTransferTimeout)
	defer cancel()
	report, err := remoteOrigin(ctx, a, remoteFeature(w.deps))
	result := workbenchResult(commandResult{err: err, code: remoteCLIResultCode(a.Command, report, err)})
	result.Remote = &report
	return result
}

// Teams supplies definition input per operation; it never replaces a shared
// process reader or permits an update without the catalog's revision checks.
func (w *Workbench) Teams(ctx context.Context, a Arguments, definition io.Reader) (TeamsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, teamsOperationTimeout)
	defer cancel()
	a.Command = "teams"
	deps := w.deps
	if definition != nil {
		if a.TeamDefinition != "" && a.TeamDefinition != "-" {
			return TeamsResponse{}, errors.New("team definition requires exactly one file or reader source")
		}
		a.TeamDefinition, deps.Input = "-", definition
	} else if a.TeamDefinition == "-" && (a.TeamAction == "register" || a.TeamAction == "update") {
		return TeamsResponse{}, errors.New("team definition requires a per-operation reader")
	}
	return teamsCommand(ctx, a, deps)
}

func (w *Workbench) Schedules(ctx context.Context, a Arguments) OperationResult {
	a.Command = "schedule"
	if scheduleReadAction(a) {
		return workbenchResult(scheduleQueryCommandContext(ctx, a, w.deps.storeDependencies()))
	}
	return workbenchInputFailure(a.Command, errors.New("schedule mutation requires an explicit operation"))
}

func (w *Workbench) ScheduleMutation(a Arguments) OperationResult {
	a.Command = "schedule"
	if scheduleReadAction(a) {
		return workbenchInputFailure(a.Command, errors.New("schedule observation requires its read operation"))
	}
	if err := validateScheduleCreateOptions(&a); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(scheduleCommandWithTeams(a, w.deps))
}

func (w *Workbench) Workspace(ctx context.Context, a Arguments) OperationResult {
	a.Command = "workspace"
	var positional []string
	if a.WorkspaceAction != "list" {
		positional = []string{a.WorkspaceID}
	}
	if err := validateWorkspaceArguments(&a, positional, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(workspaceCommandContext(ctx, a, w.deps))
}

func (w *Workbench) Checkpoint(ctx context.Context, a Arguments) OperationResult {
	a.Command = "checkpoint"
	var positional []string
	if a.CheckpointAction == "status" || a.CheckpointAction == "remove" {
		positional = []string{a.CheckpointID}
	}
	if err := validateCheckpointArguments(&a, positional, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(checkpointCommandContext(ctx, a, w.deps))
}

func (w *Workbench) Supervisor(a Arguments) OperationResult {
	a.Command = "supervisor"
	if err := a.validate(nil, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(supervisorCommand(a, w.deps))
}

func (w *Workbench) Scheduler(ctx context.Context, a Arguments) OperationResult {
	a.Command = "scheduler"
	if a.SchedulerAction != "status" && a.SchedulerAction != "stop" && a.SchedulerAction != "migrate" {
		return workbenchInputFailure(a.Command, errors.New("scheduler serve/run require an independent foreground terminal"))
	}
	if err := a.validate(nil, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(runSchedulerCommand(ctx, a, w.deps, io.Discard))
}

func (w *Workbench) Wait(ctx context.Context, a Arguments) OperationResult {
	a.Command = "wait"
	if err := validateWaitArguments(a); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(waitTasksContext(ctx, a, w.deps.storeDependencies()))
}

func (w *Workbench) Remote(ctx context.Context, a Arguments) OperationResult {
	a.Command = "remote"
	switch a.RemoteAction {
	case "list", "reconcile", "usage", "retire":
	default:
		return workbenchInputFailure(a.Command, errors.New("remote action is not an interactive operation"))
	}
	var positional []string
	if a.RemoteAction != "list" {
		positional = []string{a.TaskID}
	}
	validated, err := validateRemoteAction(a, positional)
	if err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return w.remote(ctx, validated)
}

func (w *Workbench) Device(ctx context.Context, a Arguments) OperationResult {
	a.Command = "devices"
	validated, err := validateDeviceAction(a, nil)
	if err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return w.remote(ctx, validated)
}

func (w *Workbench) Providers() ProviderResponse {
	return ProviderResponse{SchemaVersion: OutputSchemaVersion, Providers: w.deps.Catalog.Descriptions()}
}

func (w *Workbench) Capabilities(providerID string) (CapabilityResponse, error) {
	return observeCapability(w.deps.Catalog, providerID)
}

func (w *Workbench) Preflight(ctx context.Context, a Arguments) OperationResult {
	a.Command = "preflight"
	if err := a.validate(nil, a.Watch.String()); err != nil {
		return workbenchInputFailure(a.Command, err)
	}
	return workbenchResult(preflightContext(ctx, a, w.deps))
}

// Models is explicit provider discovery, not an automatic form lookup. The
// accompanying operation disposition preserves retained cleanup custody.
func (w *Workbench) Models(a Arguments) (ModelsResponse, OperationResult) {
	a.Command = "models"
	if err := a.validate(nil, a.Watch.String()); err != nil {
		return ModelsResponse{}, workbenchInputFailure(a.Command, err)
	}
	observed := observeModels(a, w.deps)
	return observed.response, workbenchResult(commandResult{code: observed.code, err: observed.err})
}

func (w *Workbench) Doctor(a Arguments) (DoctorReport, int) {
	a.Command = "doctor"
	dependent := a.Config.Model != "" || a.Config.Effort != "" || a.ReviewTarget != "" || a.Config.ReviewTarget != nil
	if err := validateDoctorValues(&a, dependent); err != nil {
		return DoctorReport{SchemaVersion: OutputSchemaVersion, Command: "doctor", Issues: []DoctorIssue{{Code: "invalid_request", Area: "request", NextStep: err.Error()}}}, 2
	}
	self, err := os.Executable()
	return observeDoctor(a, w.deps, self, err)
}
