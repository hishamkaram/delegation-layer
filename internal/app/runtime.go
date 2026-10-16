package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

const (
	// OutputSchemaVersion is the stable public control-response schema.
	OutputSchemaVersion = 1
	defaultPublisher    = "delegate"
	// MaxJSONResponseBytes bounds each complete public JSON response. The
	// writer preserves the response's authority fields and shortens only
	// diagnostic text when the encoded response would exceed this bound.
	MaxJSONResponseBytes = task.MaxControlRecordSize
)

// ErrJSONResponseTooLarge reports that authority and descriptor fields alone
// cannot fit in the complete JSON response bound.
var ErrJSONResponseTooLarge = fmt.Errorf("%w: JSON response exceeds maximum permitted size", task.ErrControlRecordTooBig)

// UsageText is the public command synopsis shared by delegate and its tests.
const UsageText = `Usage: delegate [global flags] <command> [command flags]

A durable single-machine agent delegation layer.

Commands:
  dispatch      Persist and submit one bounded provider turn
  providers     Describe compiled provider capabilities
  capabilities  Report one provider capability contract
  models        Discover native model IDs and effort choices
  status        Observe admission, liveness, and publication
  collect       Recover or read the immutable publication
  recheck       Re-evaluate a rejected task from sealed local evidence
  reply         Answer an outstanding native clarification request
  steer         Send guidance to a supported active native turn
  events        Read interaction events, optionally with a bounded watch
  cancel        Request one explicit supervisor stop
  logs          Return validated output descriptors
  preflight     Check one provider without creating a task
  continue      Continue one terminal task through its exact session
  supervisor    Inspect or migrate Delegate's private supervisor
  workspace     Inspect, verify, release, or forget a managed Git workspace
  help          Show this help message
  version       Print version information

Global flags:
  --root ABS              State root (default: user config directory)
  --pueue-config ABS      Initial supervisor configuration
  --runner ABS            Runner executable for dispatch

Supervisor recovery:
  supervisor status --json
  supervisor migrate --json

Managed Git workspaces (optional):
  dispatch ... --cwd ABS --isolation git-worktree [--base REV]
  dispatch ... --workspace ID
  workspace list [--after ID] [--limit 50] --json
  workspace status ID [--operation OP] [--watch DURATION] --json
  workspace exec ID --budget DURATION -- COMMAND ARGS
  workspace release ID [--discard] [--budget DURATION] --json
  workspace forget ID [--budget DURATION] --json
  --workspace-budget DURATION sets the finite helper budget (default: 5m).
  Source edits are excluded; landing changes belongs to the Lead.

Interactive tasks:
  dispatch ... --interactive
  continue --task ID ... --interactive
  reply ID --request REQUEST_ID --answers FILE --id MESSAGE_ID --json
  steer ID --brief FILE --id MESSAGE_ID --json
  events ID [--after SEQUENCE] [--watch DURATION] --json

Use --json on task, providers, capabilities, models, preflight, continue, and recheck commands for the versioned response.
`

// Dependencies is the explicit composition boundary for production and the
// hermetic acceptance harness. Production leaves hooks and profile selection at
// their safe defaults; acceptance supplies a compiled finite profile directly.
type Dependencies struct {
	// Catalog is the immutable provider composition for this command. When it
	// is empty, normalized constructs the production catalog once.
	Catalog                     commonprovider.Catalog
	PrepareProfile              PrepareProfile
	PrepareCandidate            commonprovider.PrepareCandidate
	PredicateRegistry           func() predicate.Registry
	SupervisorOptions           pueue.Options
	InitialSupervisorExecutable string
	RunnerExecutable            string
	PublisherVersion            string
	ExecutionHooks              execution.Hooks
	useCatalogExisting          bool
}

// storeDependencies is the narrow composition passed to read-only task-store
// commands. Profile selection, runner resolution, and supervisor mutation
// options remain outside this boundary.
type storeDependencies struct {
	predicateRegistry           func() predicate.Registry
	catalog                     commonprovider.Catalog
	supervisorOptions           pueue.Options
	initialSupervisorExecutable string
	runnerExecutable            string
	observeSupervisor           bool
}

func (d Dependencies) storeDependencies() storeDependencies {
	registry := d.PredicateRegistry
	if registry == nil && !d.Catalog.IsZero() {
		registry = d.Catalog.Registry
	}
	return storeDependencies{
		predicateRegistry:           registry,
		catalog:                     d.Catalog,
		supervisorOptions:           d.SupervisorOptions,
		initialSupervisorExecutable: d.InitialSupervisorExecutable,
		runnerExecutable:            d.RunnerExecutable,
		observeSupervisor:           true,
	}
}

func (d Dependencies) normalized() Dependencies {
	if d.Catalog.IsZero() {
		d.Catalog = NativeCatalog()
	}
	if d.PrepareCandidate == nil {
		if d.PrepareProfile != nil {
			d.PrepareCandidate = commonprovider.ReadyCandidate(d.PrepareProfile)
		} else {
			d.PrepareCandidate = d.Catalog.Candidate
			d.useCatalogExisting = true
		}
	}
	if d.PredicateRegistry == nil {
		d.PredicateRegistry = d.Catalog.Registry
	}
	if d.PublisherVersion == "" {
		d.PublisherVersion = defaultPublisher
	}
	if d.SupervisorOptions.ControlOwnerExecutable == "" {
		d.SupervisorOptions.ControlOwnerExecutable = discoverControlOwnerExecutable()
	}
	if d.SupervisorOptions.PrivateMaintenanceLauncher == nil && d.SupervisorOptions.ControlOwnerExecutable != "" {
		d.SupervisorOptions.PrivateMaintenanceLauncher = launchPrivateQueueMaintenanceOwner
	}
	if d.SupervisorOptions.SuccessfulCleanupAuthorizer == nil {
		d.SupervisorOptions.SuccessfulCleanupAuthorizer = authorizePrivateSuccessfulCleanup
	}
	if d.SupervisorOptions.PrivateTerminalEvidencePreparer == nil {
		d.SupervisorOptions.PrivateTerminalEvidencePreparer = preparePrivateTerminalEvidence
	}
	if d.SupervisorOptions.PrivateMigrationAuthorizer == nil {
		d.SupervisorOptions.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	}
	return d
}

func discoverControlOwnerExecutable() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	self = canonicalExecutablePath(self)
	candidate := self
	base := filepath.Base(self)
	if base != stateRunnerName && !strings.HasPrefix(base, stateRunnerName+"-") {
		candidate = managedRunnerFromExecutable(self)
	}
	if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || resolved != candidate {
		return ""
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return candidate
}

// Response is the bounded public control response. Payload and sealed raw
// output are described by immutable digests; live raw descriptors carry only
// rooted paths and availability. Answer bytes are streamed by a future typed
// result API rather than copied into an unbounded control object.
type Response struct {
	Interaction       *task.InteractionState     `json:"interaction,omitempty"`
	Delivery          *task.InteractionDelivery  `json:"delivery,omitempty"`
	Events            []task.InteractionEvent    `json:"events,omitempty"`
	Cursor            *uint64                    `json:"cursor,omitempty"`
	SchemaVersion     int                        `json:"schema_version"`
	Command           string                     `json:"command"`
	RootID            string                     `json:"root_id,omitempty"`
	TaskID            string                     `json:"task_id,omitempty"`
	TaskRecord        string                     `json:"task_record,omitempty"`
	ParentTaskID      string                     `json:"parent_task_id,omitempty"`
	Status            string                     `json:"status,omitempty"`
	Admission         string                     `json:"admission"`
	Liveness          string                     `json:"liveness"`
	Publication       string                     `json:"publication"`
	Outcome           *task.OutcomeRecord        `json:"outcome,omitempty"`
	Payload           *task.PayloadDescriptor    `json:"payload,omitempty"`
	Raw               []taskdir.LogDescriptor    `json:"raw,omitempty"`
	EvidenceSHA256    string                     `json:"evidence_sha256,omitempty"`
	Supervisor        *SupervisorResponse        `json:"supervisor,omitempty"`
	SupervisorControl *SupervisorControlResponse `json:"supervisor_control,omitempty"`
	Stops             []StopResponse             `json:"stops,omitempty"`
	Stop              *StopResponse              `json:"stop,omitempty"`
	Pending           *PendingResponse           `json:"pending,omitempty"`
	Capability        *CapabilityReport          `json:"capability,omitempty"`
	Continuation      *ContinuationResponse      `json:"continuation,omitempty"`
	Failure           *DispatchFailureResponse   `json:"failure,omitempty"`
	Diagnostics       []task.OutputDiagnostic    `json:"diagnostics,omitempty"`
	Recheck           *RecheckResponse           `json:"recheck,omitempty"`
	Error             string                     `json:"error,omitempty"`
	ErrorTruncated    bool                       `json:"error_truncated,omitempty"`
	Workspace         *WorkspaceResponse         `json:"workspace,omitempty"`
	Workspaces        []WorkspaceResponse        `json:"workspaces,omitempty"`
	WorkspaceCursor   string                     `json:"workspace_cursor,omitempty"`
	WorkspaceSupport  *WorkspaceSupportReport    `json:"workspace_support,omitempty"`
}

// DispatchFailureResponse gives an agent a stable recovery decision without
// exposing supervisor paths or raw process diagnostics.
type DispatchFailureResponse struct {
	Code            string                     `json:"code"`
	ReasonCode      string                     `json:"reason_code,omitempty"`
	Stage           string                     `json:"stage"`
	NextAction      string                     `json:"next_action"`
	RecoveryCommand string                     `json:"recovery_command,omitempty"`
	Inspection      *InspectionFailureResponse `json:"inspection,omitempty"`
}

// InspectionFailureResponse retains bounded, path-free evidence about an
// admission inspection failure. The task ID addresses its durable journal
// under the caller's existing state root without exposing that root.
type InspectionFailureResponse struct {
	Class         string `json:"class"`
	DeadlineState string `json:"deadline_state"`
	JournalTaskID string `json:"journal_task_id,omitempty"`
}

// ContinuationResponse is emitted when a task can be continued or when a
// continuation command creates a linked successor. Task records and sealed
// evidence remain authoritative for the detailed lifecycle.
type ContinuationResponse struct {
	Resumable       bool   `json:"resumable"`
	Mode            string `json:"mode"`
	PredecessorID   string `json:"predecessor_task_id,omitempty"`
	ContinueCommand string `json:"continue_command,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// RecheckResponse describes one immutable offline assessment while retaining
// the source task's original outcome and publication fields in Response.
type RecheckResponse struct {
	Status               string                  `json:"status"`
	AssessmentID         string                  `json:"assessment_id"`
	SourceOutcomeSHA256  string                  `json:"source_outcome_sha256"`
	SourceSealSHA256     string                  `json:"source_seal_sha256"`
	SourceEvidenceSHA256 string                  `json:"source_evidence_sha256"`
	SourcePredicate      task.PredicateRef       `json:"source_predicate"`
	Target               task.PredicateRef       `json:"target"`
	ReportPath           string                  `json:"report_path"`
	Diagnostics          []task.OutputDiagnostic `json:"diagnostics,omitempty"`
	Answer               *task.PayloadDescriptor `json:"answer,omitempty"`
	AnswerPath           string                  `json:"answer_path,omitempty"`
}

type SupervisorResponse struct {
	Matched       bool   `json:"matched"`
	State         string `json:"state"`
	NumericTaskID *int64 `json:"numeric_task_id,omitempty"`
}

// SupervisorControlResponse describes Delegate-owned supervisor recovery
// without exposing executable paths, socket paths, or process diagnostics.
type SupervisorControlResponse struct {
	Action             string `json:"action"`
	State              string `json:"state"`
	LegacyReachability string `json:"legacy_reachability"`
	ActiveWork         string `json:"active_work"`
	MigrationPhase     string `json:"migration_phase,omitempty"`
	NextAction         string `json:"next_action"`
	ReasonCode         string `json:"reason_code,omitempty"`
}

type StopResponse struct {
	RequestID        string `json:"request_id,omitempty"`
	Cause            string `json:"cause,omitempty"`
	Requested        bool   `json:"requested"`
	Matched          bool   `json:"matched"`
	Attempted        bool   `json:"attempted"`
	Action           string `json:"action,omitempty"`
	NumericTaskID    *int64 `json:"numeric_task_id,omitempty"`
	Acknowledged     *bool  `json:"acknowledged,omitempty"`
	InFlight         bool   `json:"in_flight"`
	ObservedState    string `json:"observed_state"`
	Terminated       bool   `json:"terminated"`
	Message          string `json:"message,omitempty"`
	MessageTruncated bool   `json:"message_truncated,omitempty"`
}

type PendingResponse struct {
	Kind string `json:"kind"`
}

const pendingLifetimeFinalization = "lifetime-finalization"

func markLifetimeFinalizationPending(response *Response) {
	if response == nil {
		return
	}
	response.Liveness = task.LivenessRunning.String()
	response.Pending = &PendingResponse{Kind: pendingLifetimeFinalization}
}

func lifetimeFinalizationPending(response Response) bool {
	return response.Pending != nil && response.Pending.Kind == pendingLifetimeFinalization
}

type commandResult struct {
	response Response
	code     int
	err      error
}

func newResponse(command string) Response {
	response := Response{SchemaVersion: OutputSchemaVersion, Command: command, Admission: task.AdmissionUnknown.String(), Liveness: task.LivenessUndetermined.String(), Publication: task.PublicationUnknown.String()}
	if command == "providers" || command == "capabilities" {
		response.WorkspaceSupport = workspaceSupport()
	}
	return response
}

func (r *Response) setError(err error) {
	if err != nil {
		r.Error = err.Error()
	}
}

// Run parses and executes one public delegate command.
func Run(args []string, stdout, stderr io.Writer, deps Dependencies) int {
	parsed, err := ParseArguments(args)
	if err != nil {
		if _, writeErr := fmt.Fprintf(stderr, "error: %v\n\n%s", err, UsageText); writeErr != nil {
			return 1
		}
		return 2
	}
	if parsed.Command == "help" {
		if _, err = io.WriteString(stdout, UsageText); err != nil {
			return 1
		}
		return 0
	}
	if parsed.Command == "version" {
		if _, err = fmt.Fprintf(stdout, "delegate %s\n", deps.normalized().PublisherVersion); err != nil {
			return 1
		}
		return 0
	}

	normalized := deps.normalized()
	if parsed.Command == "providers" {
		return runProviders(parsed.JSON, stdout, stderr, normalized.Catalog)
	}
	if parsed.Command == "capabilities" {
		return runCapabilities(parsed.JSON, stdout, stderr, normalized.Catalog, parsed.Provider)
	}
	if parsed.Command == "models" {
		return runModels(parsed, stdout, normalized)
	}
	result := runCommand(parsed, normalized)
	if parsed.JSON {
		if err = writeJSONResponse(stdout, parsed.Command, &result.response, result.err); err != nil {
			return 1
		}
	} else {
		if err = writeHumanResponse(stdout, result.response); err != nil {
			return 1
		}
		if result.err != nil {
			return writeCLIError(stderr, commandPresentationError(parsed.Command, result), result.code)
		}
	}
	return result.code
}

func writeJSONResponse(stdout io.Writer, command string, response *Response, resultErr error) error {
	response.setError(resultErr)
	if (command == "dispatch" || command == "continue") && resultErr != nil {
		if response.Failure == nil {
			failure := dispatchFailure(resultErr, response.TaskRecord)
			response.Failure = &failure
		}
		failure := response.Failure
		response.Error = dispatchFailureMessage(failure.Code)
		if failure.ReasonCode != "" {
			response.Error = publicSupervisorError(response.Error, resultErr).Error()
		}
	}
	return writeJSON(stdout, *response)
}

func runCommand(a Arguments, deps Dependencies) commandResult {
	switch a.Command {
	case "dispatch":
		return dispatch(a, deps)
	case "preflight":
		return preflight(a, deps)
	case "continue":
		return continueTask(a, deps)
	case "status":
		return status(a, deps)
	case "collect":
		return collect(a, deps.storeDependencies())
	case "recheck":
		return recheck(a, deps.storeDependencies())
	case "cancel":
		return cancel(a, deps)
	case "reply", "steer", "events":
		return interactionCommand(a, deps.storeDependencies())
	case "logs":
		return logs(a, deps.storeDependencies())
	case "supervisor":
		return supervisorCommand(a, deps)
	case "workspace":
		return workspaceCommand(a, deps)
	default:
		return commandResult{response: newResponse(a.Command), code: 2, err: fmt.Errorf("unknown command %q", a.Command)}
	}
}

func writeCLIError(w io.Writer, err error, code int) int {
	if err == nil {
		return code
	}
	_, writeErr := fmt.Fprintf(w, "error: %v\n", err)
	if writeErr != nil {
		return 1
	}
	return code
}

func writeHumanResponse(w io.Writer, response Response) error {
	var output strings.Builder
	writeHumanSummary(&output, response)
	writeHumanSupervisor(&output, response.SupervisorControl)
	writeHumanContinuation(&output, response.Continuation)
	writeHumanPayload(&output, response.Payload)
	writeHumanOutputDiagnostics(&output, response.Diagnostics)
	writeHumanRecheck(&output, response.Recheck)
	writeHumanRaw(&output, response.Raw)
	writeHumanStops(&output, response.Stop, response.Stops)
	writeHumanWorkspaceResponse(&output, response)
	if err := writeHumanInteraction(&output, response); err != nil {
		return err
	}
	return writeAll(w, []byte(output.String()))
}

func writeHumanSummary(output *strings.Builder, response Response) {
	fmt.Fprintf(output, "command=%s root_id=%s task_id=%s admission=%s liveness=%s publication=%s", response.Command, response.RootID, response.TaskID, response.Admission, response.Liveness, response.Publication)
	if response.Status != "" {
		fmt.Fprintf(output, " status=%s", response.Status)
	}
	output.WriteByte('\n')
}

func writeHumanSupervisor(output *strings.Builder, control *SupervisorControlResponse) {
	if control == nil {
		return
	}
	fmt.Fprintf(output, "supervisor action=%s state=%s legacy_reachability=%s active_work=%s next_action=%s", control.Action, control.State, control.LegacyReachability, control.ActiveWork, control.NextAction)
	if control.MigrationPhase != "" {
		fmt.Fprintf(output, " migration_phase=%s", control.MigrationPhase)
	}
	output.WriteByte('\n')
}

func writeHumanContinuation(output *strings.Builder, continuation *ContinuationResponse) {
	if continuation == nil {
		return
	}
	fmt.Fprintf(output, "continuation resumable=%t mode=%s", continuation.Resumable, continuation.Mode)
	if continuation.PredecessorID != "" {
		fmt.Fprintf(output, " predecessor_task_id=%s", continuation.PredecessorID)
	}
	if continuation.ContinueCommand != "" {
		fmt.Fprintf(output, " command=%s", continuation.ContinueCommand)
	}
	output.WriteByte('\n')
}

func writeHumanPayload(output *strings.Builder, payload *task.PayloadDescriptor) {
	if payload != nil {
		fmt.Fprintf(output, "payload=%s length=%d sha256=%s\n", payload.Basename, payload.Length, payload.SHA256)
	}
}

func writeHumanRecheck(output *strings.Builder, recheck *RecheckResponse) {
	if recheck == nil {
		return
	}
	fmt.Fprintf(output, "recheck status=%s assessment_id=%s report=%s", recheck.Status, recheck.AssessmentID, recheck.ReportPath)
	if recheck.AnswerPath != "" {
		fmt.Fprintf(output, " answer=%s", recheck.AnswerPath)
	}
	output.WriteByte('\n')
}

func writeHumanRaw(output *strings.Builder, raw []taskdir.LogDescriptor) {
	for _, descriptor := range raw {
		fmt.Fprintf(output, "raw path=%s available=%t sealed=%t", descriptor.Path, descriptor.Available, descriptor.Sealed)
		if descriptor.Size != nil {
			fmt.Fprintf(output, " size=%d", *descriptor.Size)
		}
		if descriptor.SHA256 != "" {
			fmt.Fprintf(output, " sha256=%s", descriptor.SHA256)
		}
		output.WriteByte('\n')
	}
}

func writeHumanStops(output *strings.Builder, stop *StopResponse, stops []StopResponse) {
	if stop != nil {
		writeHumanStop(output, "stop", *stop)
	}
	for _, item := range stops {
		writeHumanStop(output, "stop", item)
	}
}

func writeHumanOutputDiagnostics(output *strings.Builder, diagnostics []task.OutputDiagnostic) {
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(output, "diagnostic code=%s message=%s", diagnostic.Code, diagnostic.Message)
		if diagnostic.Field != "" {
			fmt.Fprintf(output, " field=%s", diagnostic.Field)
		}
		if diagnostic.Event != 0 {
			fmt.Fprintf(output, " event=%d", diagnostic.Event)
		}
		if diagnostic.Offset != 0 {
			fmt.Fprintf(output, " offset=%d", diagnostic.Offset)
		}
		output.WriteByte('\n')
	}
}

func writeHumanStop(output *strings.Builder, prefix string, stop StopResponse) {
	fmt.Fprintf(output, "%s request_id=%s requested=%t matched=%t attempted=%t in_flight=%t observed_state=%s terminated=%t", prefix, stop.RequestID, stop.Requested, stop.Matched, stop.Attempted, stop.InFlight, stop.ObservedState, stop.Terminated)
	if stop.Action != "" {
		fmt.Fprintf(output, " action=%s", stop.Action)
	}
	if stop.NumericTaskID != nil {
		fmt.Fprintf(output, " numeric_task_id=%d", *stop.NumericTaskID)
	}
	if stop.Acknowledged != nil {
		fmt.Fprintf(output, " acknowledged=%t", *stop.Acknowledged)
	}
	output.WriteByte('\n')
}

func writeJSON(w io.Writer, response Response) error {
	data, err := encodeJSONResponse(response)
	if err != nil {
		return err
	}
	if len(data) > MaxJSONResponseBytes {
		for budget := 4096; budget >= 0; budget /= 2 {
			candidate := truncateDiagnostics(response, budget)
			data, err = encodeJSONResponse(candidate)
			if err != nil {
				return err
			}
			if len(data) <= MaxJSONResponseBytes {
				break
			}
			if budget == 0 {
				return ErrJSONResponseTooLarge
			}
		}
	}
	if len(data) > MaxJSONResponseBytes {
		return ErrJSONResponseTooLarge
	}
	return writeAll(w, data)
}

func encodeJSONResponse(response Response) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func truncateDiagnostics(response Response, budget int) Response {
	candidate := response
	var truncated bool
	candidate.Error, truncated = truncateDiagnostic(response.Error, budget)
	candidate.ErrorTruncated = response.ErrorTruncated || truncated
	if response.Stops != nil {
		candidate.Stops = append([]StopResponse(nil), response.Stops...)
		for i := range candidate.Stops {
			candidate.Stops[i].Message, truncated = truncateDiagnostic(response.Stops[i].Message, budget)
			candidate.Stops[i].MessageTruncated = response.Stops[i].MessageTruncated || truncated
		}
	}
	if response.Stop != nil {
		stop := *response.Stop
		stop.Message, truncated = truncateDiagnostic(response.Stop.Message, budget)
		stop.MessageTruncated = response.Stop.MessageTruncated || truncated
		candidate.Stop = &stop
	}
	if response.Diagnostics != nil {
		candidate.Diagnostics = append([]task.OutputDiagnostic(nil), response.Diagnostics...)
		for i := range candidate.Diagnostics {
			candidate.Diagnostics[i].Message, _ = truncateDiagnostic(response.Diagnostics[i].Message, budget)
		}
	}
	return candidate
}

func truncateDiagnostic(value string, budget int) (string, bool) {
	if value == "" || len(value) <= budget {
		return value, false
	}
	const marker = "[truncated]"
	if budget <= len(marker) {
		return marker, true
	}
	limit := budget - len(marker)
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + marker, true
}

func resolveRoot(raw string) (string, error) {
	if raw == "" {
		var err error
		raw, err = config.ResolveDefaultRoot()
		if err != nil {
			return "", err
		}
	}
	return config.CanonicalizePath(raw)
}

func (d storeDependencies) registry() predicate.Registry {
	if d.predicateRegistry != nil {
		return d.predicateRegistry()
	}
	return NativePredicates()
}

func (d storeDependencies) providerCatalog() commonprovider.Catalog {
	if d.catalog.IsZero() {
		return NativeCatalog()
	}
	return d.catalog
}

func openStore(root string, deps storeDependencies, create bool) (*taskdir.Store, error) {
	registry := deps.registry()
	if create {
		return taskdir.InitStoreWithPredicates(root, registry)
	}
	return taskdir.OpenStoreWithPredicates(root, registry)
}

func openStoreContext(ctx context.Context, root string, deps storeDependencies, create bool) (*taskdir.Store, error) {
	if create {
		return taskdir.InitStoreWithPredicatesContext(ctx, root, deps.registry())
	}
	return taskdir.OpenStoreWithPredicatesContext(ctx, root, deps.registry())
}

func readBrief(path string) (data []byte, resultErr error) {
	if _, err := config.ValidateBriefFile(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	f, err := openBriefForRead(path)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, config.ErrBriefNotRegular
	}
	data, err = task.ReadBounded(f, task.MaxBriefSize)
	if err != nil {
		return nil, err
	}
	final, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if final.Size() != opened.Size() || len(data) != int(opened.Size()) || !final.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("brief changed while it was being read")
	}
	if len(data) == 0 {
		return nil, errors.New("brief must be nonempty")
	}
	return data, nil
}

func openBriefForRead(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, config.ErrBriefNotRegular
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		return nil, errors.Join(errors.New("opening brief file descriptor"), unix.Close(fd))
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(config.ErrBriefNotRegular, f.Close())
	}
	return f, nil
}

func buildRequest(rootID, id, provider, cwd string, requested task.TaskConfig, brief []byte, prior *task.PriorSession) (task.TaskRecord, error) {
	budget, err := config.ParseBudgetForProvider(provider, requested.Budget)
	if err != nil {
		return task.TaskRecord{}, err
	}
	req := task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, TaskID: id, Provider: provider, Mode: requested.Permission, CanonicalCwd: cwd, RequestedConfig: requested, BudgetNanos: int64(budget), PriorSession: prior, BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	if err = task.NormalizeRequestedConfig(&req); err != nil {
		return task.TaskRecord{}, err
	}
	return req, nil
}

func prepareProfile(deps Dependencies, req task.TaskRecord) (PreparedProfile, error) {
	candidate, err := deps.normalized().PrepareCandidate(req)
	if err != nil {
		return PreparedProfile{}, err
	}
	if candidate.Inspection != nil || candidate.Finalize == nil {
		return PreparedProfile{}, ErrProfileUnavailable
	}
	profile, err := candidate.Finalize(nil, time.Now())
	if err != nil {
		return PreparedProfile{}, err
	}
	if err = profile.Validate(req); err != nil {
		return PreparedProfile{}, err
	}
	return profile, nil
}

func resolveInitialConfig(a Arguments) (string, error) {
	path := a.PueueConfig
	if path == "" {
		path = os.Getenv("DELEGATE_PUEUE_CONFIG")
	}
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", pueue.ErrConfiguration
	}
	return path, nil
}

func resolveInitialExecutable(deps Dependencies) (string, error) {
	path := deps.InitialSupervisorExecutable
	if path == "" {
		var err error
		path, err = exec.LookPath("pueue")
		if err != nil {
			return "", fmt.Errorf("resolving pueue executable: %w", err)
		}
	}
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = absolute
	}
	return path, nil
}

func resolveBundledExecutable(deps Dependencies, name string) (string, error) {
	directory, err := bundledSupervisorDirectory(deps)
	if err != nil {
		return "", err
	}
	return resolveSupervisorExecutable(filepath.Join(directory, name), name)
}

func resolveBundledExecutables(deps Dependencies) (client, daemon string, resultErr error) {
	return resolveBundledExecutablesWithPath(deps, true)
}

func resolveBundledExecutablesWithPath(deps Dependencies, allowPath bool) (client, daemon string, resultErr error) {
	directory, err := bundledSupervisorDirectory(deps)
	if err != nil {
		return "", "", err
	}
	client, daemon, found, statePairErr := resolveStateSupervisorPair(directory)
	if found {
		return client, daemon, statePairErr
	}
	client, daemon, clientErr := resolveSupervisorPair(directory)
	if clientErr == nil {
		return client, daemon, nil
	}
	if deps.InitialSupervisorExecutable != "" || bundledSupervisorPairHasEntry(directory) {
		return "", "", clientErr
	}
	client, daemon, found, libexecErr := resolveBundledLibexecPair(directory)
	if found {
		return client, daemon, libexecErr
	}
	if !allowPath {
		return "", "", clientErr
	}

	pathClient, pathErr := exec.LookPath("pueue")
	if pathErr != nil {
		return "", "", clientErr
	}
	client, err = resolveSupervisorExecutable(pathClient, "pueue")
	if err != nil {
		return "", "", err
	}
	daemon, err = resolveSupervisorExecutable(filepath.Join(filepath.Dir(client), "pueued"), "pueued")
	if err != nil {
		return "", "", fmt.Errorf("%w: pueue and pueued must be installed as one executable pair: %w", pueue.ErrConfiguration, err)
	}
	return client, daemon, nil
}

func resolvePrivateSupervisorExecutables(root string, deps Dependencies) (client, daemon string, resultErr error) {
	stateClient, stateDaemon, found, stateErr := resolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if stateErr != nil {
		return "", "", stateErr
	}
	if found {
		return stateClient, stateDaemon, nil
	}
	return resolveBundledExecutables(deps)
}

// resolveCurrentPrivateSupervisorExecutables selects the pair shipped by the
// active installation for successor inventory and explicit migration work. It
// does not select the ordinary active generation: pueue's completed published
// binding remains authoritative until canonical migration publishes a
// replacement, while a fresh root may bootstrap the staged pair.
func resolveCurrentPrivateSupervisorExecutables(root string, deps Dependencies) (client, daemon string, resultErr error) {
	client, daemon, bundledErr := resolveBundledExecutablesWithPath(deps, false)
	if bundledErr == nil {
		return client, daemon, nil
	}
	stateClient, stateDaemon, found, stateErr := resolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if stateErr != nil {
		return "", "", errors.Join(bundledErr, stateErr)
	}
	if found {
		return stateClient, stateDaemon, nil
	}
	return resolveBundledExecutables(deps)
}

func resolveSupervisorPair(directory string) (client, daemon string, resultErr error) {
	client, err := resolveSupervisorExecutable(filepath.Join(directory, "pueue"), "pueue")
	if err != nil {
		return "", "", err
	}
	daemon, err = resolveSupervisorExecutable(filepath.Join(directory, "pueued"), "pueued")
	if err != nil {
		return "", "", fmt.Errorf("%w: pueue and pueued must be installed as one executable pair: %w", pueue.ErrConfiguration, err)
	}
	return client, daemon, nil
}

func bundledSupervisorPairHasEntry(directory string) bool {
	for _, name := range []string{"pueue", "pueued"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func resolveBundledLibexecPair(binaryDirectory string) (client, daemon string, found bool, resultErr error) {
	directory := filepath.Clean(filepath.Join(binaryDirectory, "..", "libexec"))
	client, daemon, err := resolveSupervisorPair(directory)
	if err == nil {
		return client, daemon, true, nil
	}
	if bundledSupervisorPairHasEntry(directory) {
		return "", "", true, err
	}
	return "", "", false, nil
}

func bundledSupervisorDirectory(deps Dependencies) (string, error) {
	if deps.InitialSupervisorExecutable != "" {
		return filepath.Dir(deps.InitialSupervisorExecutable), nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self = canonicalExecutablePath(self)
	return filepath.Dir(self), nil
}

func canonicalExecutablePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func managedRunnerFromExecutable(self string) string {
	return filepath.Join(filepath.Dir(canonicalExecutablePath(self)), "delegate-run")
}

func resolveSupervisorExecutable(candidate, name string) (string, error) {
	path, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %s executable is unavailable: %w", pueue.ErrConfiguration, name, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%w: inspect %s executable: %w", pueue.ErrConfiguration, name, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: %s executable must be executable regular file", pueue.ErrConfiguration, name)
	}
	return path, nil
}

func resolveRunner(raw string, deps Dependencies) (string, error) {
	if raw == "" {
		raw = deps.RunnerExecutable
	}
	if raw == "" {
		self, err := os.Executable()
		if err != nil {
			return "", err
		}
		raw = managedRunnerFromExecutable(self)
	}
	if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", pueue.ErrConfiguration
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", err
	}
	if resolved != raw {
		return "", pueue.ErrConfiguration
	}
	info, err := os.Stat(raw)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", pueue.ErrConfiguration
	}
	return raw, nil
}

func resolveTaskRunner(raw string, deps Dependencies, meta *task.MetaRecord) (string, error) {
	if raw == "" && meta != nil && meta.RunnerExecutable != "" {
		raw = meta.RunnerExecutable
	}
	return resolveRunner(raw, deps)
}

func bindInitial(a Arguments, deps Dependencies) (*pueue.Client, error) {
	return bindInitialWithOptions(a, deps, "", deps.SupervisorOptions)
}

func newSupervisorClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, recoverPrivate bool, initialSupervisorExecutable string) (*pueue.Client, error) {
	if recoverPrivate && pueue.IsPrivateConfig(root, saved.ConfigPath) {
		if ctx == nil {
			return nil, context.Canceled
		}
		client, err := recoverSavedPrivateSupervisor(ctx, root, saved, options, initialSupervisorExecutable)
		if err == nil || errors.Is(err, pueue.ErrInFlight) || ctx.Err() != nil {
			return client, err
		}
		if !errors.Is(err, pueue.ErrBinding) && !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) && !errors.Is(err, os.ErrNotExist) {
			return client, err
		}
		if client != nil {
			err = errors.Join(err, client.CloseAfterPending(err))
		}
		if reconcileErr := reconcileSavedSupervisor(ctx, root, options); reconcileErr != nil {
			return nil, errors.Join(err, reconcileErr)
		}
		return recoverSavedPrivateSupervisor(ctx, root, saved, options, initialSupervisorExecutable)
	}
	return pueue.NewClient(saved, options)
}

func recoverSavedPrivateSupervisor(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, initialSupervisorExecutable string) (*pueue.Client, error) {
	clientExecutable, daemonExecutable, err := resolveRecoveryPrivateSupervisorExecutables(root, saved, initialSupervisorExecutable)
	if err != nil {
		return nil, err
	}
	return pueue.RecoverPrivate(ctx, root, saved, clientExecutable, daemonExecutable, options)
}

// newSupervisorControlClient proves the task's exact saved endpoint ready before
// a mutating queue command. If that endpoint is unavailable, completed private
// migration evidence may recover the current successor before the one control
// command is sent. An uncertain probe and a failed control are never retried.
func newSupervisorControlClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, initialSupervisorExecutable string) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	direct, directErr := pueue.BindSaved(ctx, saved, options)
	if directErr == nil {
		directErr = direct.Ready(ctx)
		if directErr == nil {
			return direct, nil
		}
	}
	if direct != nil {
		directErr = errors.Join(directErr, direct.CloseAfterPending(directErr))
	}
	if errors.Is(directErr, pueue.ErrInFlight) || ctx.Err() != nil || !pueue.IsPrivateConfig(root, saved.ConfigPath) {
		return nil, directErr
	}
	recovered, recoveryErr := newSupervisorClient(ctx, root, saved, options, true, initialSupervisorExecutable)
	if recoveryErr != nil {
		return nil, errors.Join(directErr, recoveryErr)
	}
	return recovered, nil
}

func resolveRecoveryPrivateSupervisorExecutables(root string, saved task.SupervisorRef, initialSupervisorExecutable string) (client, daemon string, resultErr error) {
	if privateSupervisorExecutablesAvailable(saved.ClientExecutable, saved.DaemonExecutable) {
		if err := validateSavedStateSupervisorPair(root, saved); err != nil {
			return "", "", fmt.Errorf("%w: saved private supervisor pair failed integrity validation: %w", pueue.ErrBinding, err)
		}
		return saved.ClientExecutable, saved.DaemonExecutable, nil
	}
	client, daemon, resolveErr := resolvePrivateSupervisorExecutables(root, Dependencies{InitialSupervisorExecutable: initialSupervisorExecutable})
	if resolveErr == nil {
		return client, daemon, nil
	}
	return "", "", fmt.Errorf("%w: resolve private supervisor executables: %w", pueue.ErrBinding, resolveErr)
}

func privateSupervisorExecutablesAvailable(clientExecutable, daemonExecutable string) bool {
	if clientExecutable == "" || daemonExecutable == "" {
		return false
	}
	if _, err := exec.LookPath(clientExecutable); err != nil {
		return false
	}
	if _, err := exec.LookPath(daemonExecutable); err != nil {
		return false
	}
	return true
}

func bindInitialWithOptions(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options) (*pueue.Client, error) {
	return bindInitialWithStart(a, deps, root, supervisorOptions, true)
}

func bindInitialForAdmission(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options, deferPrivateStart bool) (*pueue.Client, error) {
	return bindInitialWithStart(a, deps, root, supervisorOptions, !deferPrivateStart)
}

func bindInitialWithStart(a Arguments, deps Dependencies, root string, supervisorOptions pueue.Options, startPrivate bool) (*pueue.Client, error) {
	if a.savedSupervisor != nil {
		return newSupervisorClient(context.Background(), root, *a.savedSupervisor, supervisorOptions, true, deps.InitialSupervisorExecutable)
	}
	configPath, err := resolveInitialConfig(a)
	if err != nil {
		return nil, err
	}
	if configPath != "" && root != "" && pueue.IsPrivateConfig(root, configPath) {
		return nil, fmt.Errorf("%w: explicit supervisor config collides with the private state-rooted supervisor", pueue.ErrConfiguration)
	}
	if configPath == "" {
		if root == "" {
			return nil, fmt.Errorf("%w: state root is required for the private supervisor", pueue.ErrConfiguration)
		}
		if !startPrivate {
			clientExecutable, daemonExecutable, resolveErr := resolvePrivateSupervisorExecutables(root, deps)
			if resolveErr != nil {
				return nil, resolveErr
			}
			return pueue.PreparePrivate(context.Background(), clientExecutable, daemonExecutable, root, supervisorOptions)
		}
		return bindPrivateSupervisorWithOptions(root, deps, supervisorOptions)
	}
	executable, err := resolveInitialExecutable(deps)
	if err != nil {
		return nil, err
	}
	return pueue.Bind(context.Background(), executable, configPath, supervisorOptions)
}

func bindPrivateSupervisorWithOptions(root string, deps Dependencies, supervisorOptions pueue.Options) (*pueue.Client, error) {
	return bindPrivateSupervisorWithContext(context.Background(), root, deps, supervisorOptions)
}

func bindPrivateSupervisorWithContext(ctx context.Context, root string, deps Dependencies, supervisorOptions pueue.Options) (*pueue.Client, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: state root is required for the private supervisor", pueue.ErrConfiguration)
	}
	clientExecutable, daemonExecutable, err := resolvePrivateSupervisorExecutables(root, deps)
	if err != nil {
		return nil, err
	}
	return pueue.BindPrivate(ctx, clientExecutable, daemonExecutable, root, supervisorOptions)
}

func newMeta(req task.TaskRecord, profile PreparedProfile, supervisor task.SupervisorRef, runner, ownership, publisher string) task.MetaRecord {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: req.RootID, TaskID: req.TaskID, RequestedConfig: req.RequestedConfig, EffectiveConfig: profile.Effective, Containment: profile.Effective.Containment, Approval: profile.Effective.Approval, ProviderExecutable: profile.Plan.Executable, ProviderVersion: profile.ObservedVersion, RuntimePreflightSHA256: profile.RuntimePreflightSHA256, RunnerExecutable: runner, RunnerOwnership: ownership, PublisherBuild: publisher, PublisherVersion: publisher, Environment: cloneEnvironment(profile.Plan.Environment), EnvironmentRecorded: profile.Plan.Environment != nil, Predicate: profile.Plan.Predicate, InputFiles: profile.Plan.InputFiles, OutputArtifacts: profile.Plan.OutputArtifacts, OutputWriterContract: profile.Plan.OutputWriterContract, SupervisorConfig: supervisor, CreatedAt: now}
}

func cloneEnvironment(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func taskIdentity(req *task.TaskRecord, meta *task.MetaRecord, specHash, metaHash string, receipt *task.SupervisorReceipt) pueue.Identity {
	identity := pueue.Identity{RootID: req.RootID, TaskID: req.TaskID, SpecSHA256: specHash, MetaSHA256: metaHash}
	if receipt != nil {
		id := receipt.NumericTaskID
		identity.NumericTaskID = &id
	}
	return identity
}

func supervisorResponse(observation pueue.Observation) *SupervisorResponse {
	var id *int64
	if observation.Matched {
		id = &observation.NumericTaskID
	} else {
		id = copyTaskID(observation.Identity.NumericTaskID)
	}
	return &SupervisorResponse{Matched: observation.Matched, State: string(observation.State), NumericTaskID: id}
}

func pendingResponse(observation pueue.Observation) *PendingResponse {
	if observation.Pending == nil {
		return nil
	}
	return &PendingResponse{Kind: "supervisor-command"}
}

func statusStrings(insp *taskdir.TaskInspection) (publication string, outcome *task.OutcomeRecord) {
	publication = insp.Publication.String()
	if insp.Outcome != nil {
		copy := *insp.Outcome
		outcome = &copy
	}
	return publication, outcome
}

func fillInspection(response *Response, insp *taskdir.TaskInspection) {
	if insp == nil {
		return
	}
	response.Publication, response.Outcome = statusStrings(insp)
	if insp.Outcome != nil {
		payload := insp.Outcome.Payload
		response.Payload = &payload
		response.EvidenceSHA256 = insp.Outcome.EvidenceSHA256
		applyOutputDiagnostics(response, insp.Outcome.Diagnostics)
	}
}

func applyOutputDiagnostics(response *Response, diagnostics []task.OutputDiagnostic) {
	if response == nil || len(diagnostics) == 0 {
		return
	}
	response.Diagnostics = task.CloneOutputDiagnostics(diagnostics)
	rejected := response.Outcome != nil && response.Outcome.Verdict == task.VerdictRejected
	if response.Recheck != nil {
		rejected = response.Recheck.Status == task.RecheckStatusRejected
	}
	if rejected && response.Error == "" {
		first := diagnostics[0]
		response.Error = first.Code + ": " + first.Message
	}
}

func requestHashes(td *taskdir.TaskDir) (req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, err error) {
	return td.PreparedIdentity()
}

func mapPueueState(state pueue.State) string {
	switch state {
	case pueue.StateUnknown:
		return task.LivenessUndetermined.String()
	case pueue.StateQueued, pueue.StateStashed:
		return task.LivenessRunning.String()
	case pueue.StateRunning:
		return task.LivenessRunning.String()
	case pueue.StateEnded:
		return task.LivenessEnded.String()
	default:
		return task.LivenessUndetermined.String()
	}
}

func isTerminal(insp *taskdir.TaskInspection) bool {
	return insp != nil && (insp.Publication == task.PublicationCommitted || insp.Publication == task.PublicationRejected)
}

func classifyCode(err error, fallback int) int {
	if err == nil {
		return 0
	}
	for _, candidate := range []error{config.ErrInvalidBudget, config.ErrInvalidMode, config.ErrUnsupportedProvider, config.ErrInvalidCwd, config.ErrRootWorkspaceOverlap, config.ErrTokenBudgetRejected, config.ErrRawArgvRejected, config.ErrBriefNotFound, config.ErrBriefNotRegular, config.ErrBriefTooLarge, task.ErrInvalidTaskID, task.ErrInvalidRootID, task.ErrRequestConflict, task.ErrIdentityMismatch, task.ErrInvalidPermit, task.ErrIncompatiblePredicate, pueue.ErrConfiguration} {
		if errors.Is(err, candidate) {
			return 2
		}
	}
	if errors.Is(err, pueue.ErrBinding) || errors.Is(err, inspection.ErrAdmissionExpired) {
		return 1
	}
	if errors.Is(err, ErrProfileUnavailable) {
		return 2
	}
	return fallback
}

func validateRootAndCwd(root, cwd string) (string, string, error) {
	return config.ValidateDirectories(root, cwd)
}

func sameRequest(a, b *task.TaskRecord) bool {
	return task.CompareRequests(a, b)
}

// sameExistingRequest preserves the immutable request recorded by an earlier
// dispatch when a retry omits the budget. Defaults are resolved when a new
// task is admitted, but an omitted retry option must remain idempotent across a
// default change; an explicit budget still has to match exactly.
func sameExistingRequest(args Arguments, request, existing *task.TaskRecord) bool {
	if sameRequest(request, existing) {
		return true
	}
	if args.Config.Budget == "" && request != nil && existing != nil {
		candidate := *request
		candidate.BudgetNanos = existing.BudgetNanos
		candidate.RequestedConfig.Budget = existing.RequestedConfig.Budget
		return sameRequest(&candidate, existing)
	}
	return false
}

func continuationExpectation(req *task.TaskRecord) task.SessionExpectation {
	if req == nil || req.PriorSession == nil {
		return task.SessionExpectation{}
	}
	return task.SessionExpectation{Required: true, ID: req.PriorSession.ConversationID}
}

func copyTaskID(id *int64) *int64 {
	if id == nil {
		return nil
	}
	copied := *id
	return &copied
}

func describeStopRecord(record taskdir.StopRecord) StopResponse {
	result := StopResponse{}
	if record.Request != nil {
		result.RequestID = record.Request.RequestID
		result.Cause = record.Request.Cause
		result.Requested = true
		result.NumericTaskID = copyTaskID(record.Request.NumericTaskID)
	}
	if record.Reply != nil {
		result.Action = record.Reply.Action
		result.Acknowledged = boolPointer(record.Reply.Acknowledged)
		result.Message = record.Reply.Message
		result.Attempted = true
	}
	if record.Observation != nil {
		result.ObservedState = record.Observation.State
		result.Terminated = record.Observation.Terminated
	}
	return result
}

func boolPointer(value bool) *bool {
	return &value
}

func mergeCommandClose(result *commandResult, closeFn func() error) {
	if err := closeFn(); err != nil {
		result.err = errors.Join(result.err, err)
		result.code = 1
		// The command error owns failure authority. A new close fault
		// invalidates recovery selected before closing; the wire projection
		// must classify the complete error rather than an earlier pending one.
		result.response.Failure = nil
		result.response.setError(result.err)
	}
}
