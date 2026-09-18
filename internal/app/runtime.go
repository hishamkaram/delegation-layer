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
  status        Observe admission, liveness, and publication
  collect       Recover or read the immutable publication
  cancel        Request one explicit supervisor stop
  logs          Return validated output descriptors
  help          Show this help message
  version       Print version information

Global flags:
  --root ABS              State root (default: user config directory)
  --pueue-config ABS      Initial supervisor configuration
  --runner ABS            Runner executable for dispatch

Use --json on task, providers, and capabilities commands for the versioned response.
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
}

// storeDependencies is the narrow composition passed to read-only task-store
// commands. Profile selection, runner resolution, and supervisor mutation
// options remain outside this boundary.
type storeDependencies struct {
	predicateRegistry func() predicate.Registry
}

func (d Dependencies) storeDependencies() storeDependencies {
	registry := d.PredicateRegistry
	if registry == nil && !d.Catalog.IsZero() {
		registry = d.Catalog.Registry
	}
	return storeDependencies{predicateRegistry: registry}
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
		}
	}
	if d.PredicateRegistry == nil {
		d.PredicateRegistry = d.Catalog.Registry
	}
	if d.PublisherVersion == "" {
		d.PublisherVersion = defaultPublisher
	}
	return d
}

// Response is the bounded public control response. Payload and sealed raw
// output are described by immutable digests; live raw descriptors carry only
// rooted paths and availability. Answer bytes are streamed by a future typed
// result API rather than copied into an unbounded control object.
type Response struct {
	SchemaVersion  int                     `json:"schema_version"`
	Command        string                  `json:"command"`
	RootID         string                  `json:"root_id,omitempty"`
	TaskID         string                  `json:"task_id,omitempty"`
	Admission      string                  `json:"admission"`
	Liveness       string                  `json:"liveness"`
	Publication    string                  `json:"publication"`
	Outcome        *task.OutcomeRecord     `json:"outcome,omitempty"`
	Payload        *task.PayloadDescriptor `json:"payload,omitempty"`
	Raw            []taskdir.LogDescriptor `json:"raw,omitempty"`
	EvidenceSHA256 string                  `json:"evidence_sha256,omitempty"`
	Supervisor     *SupervisorResponse     `json:"supervisor,omitempty"`
	Stops          []StopResponse          `json:"stops,omitempty"`
	Stop           *StopResponse           `json:"stop,omitempty"`
	Pending        *PendingResponse        `json:"pending,omitempty"`
	Capability     *CapabilityReport       `json:"capability,omitempty"`
	Error          string                  `json:"error,omitempty"`
	ErrorTruncated bool                    `json:"error_truncated,omitempty"`
}

type SupervisorResponse struct {
	Matched       bool   `json:"matched"`
	State         string `json:"state"`
	NumericTaskID *int64 `json:"numeric_task_id,omitempty"`
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

type commandResult struct {
	response Response
	code     int
	err      error
}

func newResponse(command string) Response {
	return Response{SchemaVersion: OutputSchemaVersion, Command: command, Admission: task.AdmissionUnknown.String(), Liveness: task.LivenessUndetermined.String(), Publication: task.PublicationUnknown.String()}
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
	result := runCommand(parsed, normalized)
	if parsed.JSON {
		result.response.setError(result.err)
		if err = writeJSON(stdout, result.response); err != nil {
			return 1
		}
	} else {
		if err = writeHumanResponse(stdout, result.response); err != nil {
			return 1
		}
		if result.err != nil {
			return writeCLIError(stderr, result.err, result.code)
		}
	}
	return result.code
}

func runCommand(a Arguments, deps Dependencies) commandResult {
	switch a.Command {
	case "dispatch":
		return dispatch(a, deps)
	case "status":
		return status(a, deps)
	case "collect":
		return collect(a, deps.storeDependencies())
	case "cancel":
		return cancel(a, deps)
	case "logs":
		return logs(a, deps.storeDependencies())
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
	fmt.Fprintf(&output, "command=%s root_id=%s task_id=%s admission=%s liveness=%s publication=%s\n", response.Command, response.RootID, response.TaskID, response.Admission, response.Liveness, response.Publication)
	if response.Payload != nil {
		fmt.Fprintf(&output, "payload=%s length=%d sha256=%s\n", response.Payload.Basename, response.Payload.Length, response.Payload.SHA256)
	}
	for _, descriptor := range response.Raw {
		fmt.Fprintf(&output, "raw path=%s available=%t sealed=%t", descriptor.Path, descriptor.Available, descriptor.Sealed)
		if descriptor.Size != nil {
			fmt.Fprintf(&output, " size=%d", *descriptor.Size)
		}
		if descriptor.SHA256 != "" {
			fmt.Fprintf(&output, " sha256=%s", descriptor.SHA256)
		}
		output.WriteByte('\n')
	}
	if response.Stop != nil {
		writeHumanStop(&output, "stop", *response.Stop)
	}
	for _, stop := range response.Stops {
		writeHumanStop(&output, "stop", stop)
	}
	return writeAll(w, []byte(output.String()))
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

func openStore(root string, deps storeDependencies, create bool) (*taskdir.Store, error) {
	registry := deps.registry()
	if create {
		return taskdir.InitStoreWithPredicates(root, registry)
	}
	return taskdir.OpenStoreWithPredicates(root, registry)
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
	budget, err := config.ParseBudget(requested.Budget)
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
		return "", fmt.Errorf("%w: initial pueue config is required via --pueue-config or DELEGATE_PUEUE_CONFIG", pueue.ErrConfiguration)
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

func resolveRunner(raw string, deps Dependencies) (string, error) {
	if raw == "" {
		raw = deps.RunnerExecutable
	}
	if raw == "" {
		self, err := os.Executable()
		if err != nil {
			return "", err
		}
		raw = filepath.Join(filepath.Dir(self), "delegate-run")
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

func bindInitial(a Arguments, deps Dependencies) (*pueue.Client, error) {
	return bindInitialWithOptions(a, deps, deps.SupervisorOptions)
}

func bindInitialWithOptions(a Arguments, deps Dependencies, supervisorOptions pueue.Options) (*pueue.Client, error) {
	configPath, err := resolveInitialConfig(a)
	if err != nil {
		return nil, err
	}
	executable, err := resolveInitialExecutable(deps)
	if err != nil {
		return nil, err
	}
	return pueue.Bind(context.Background(), executable, configPath, supervisorOptions)
}

func newMeta(req task.TaskRecord, profile PreparedProfile, supervisor task.SupervisorRef, publisher string) task.MetaRecord {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: req.RootID, TaskID: req.TaskID, RequestedConfig: req.RequestedConfig, EffectiveConfig: profile.Effective, Containment: profile.Effective.Containment, Approval: profile.Effective.Approval, ProviderExecutable: profile.Plan.Executable, ProviderVersion: profile.ObservedVersion, PublisherBuild: publisher, PublisherVersion: publisher, Predicate: profile.Plan.Predicate, InputFiles: profile.Plan.InputFiles, OutputArtifacts: profile.Plan.OutputArtifacts, OutputWriterContract: profile.Plan.OutputWriterContract, SupervisorConfig: supervisor, CreatedAt: now}
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
	}
}

func requestHashes(td *taskdir.TaskDir) (req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, err error) {
	return td.PreparedIdentity()
}

func mapPueueState(state pueue.State) string {
	switch state {
	case pueue.StateUnknown:
		return task.LivenessUndetermined.String()
	case pueue.StateQueued:
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
		result.response.setError(result.err)
	}
}
