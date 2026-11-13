package control

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const (
	controlOwnerMode             = "--supervisor-control-owner"
	controlOwnerSocketFD         = 3
	controlOwnerWriteTimeout     = 2 * time.Second
	maxControlOwnerRequestBytes  = 256 << 10
	maxControlOwnerResponseBytes = 8 << 20
)

type controlOwnerRequest struct {
	SchemaVersion int                   `json:"schema_version"`
	CommandID     string                `json:"command_id"`
	Executable    string                `json:"executable"`
	Arguments     []string              `json:"arguments"`
	Directory     string                `json:"directory"`
	StatusVersion string                `json:"status_version,omitempty"`
	Shutdown      *controlOwnerShutdown `json:"shutdown,omitempty"`
}

type controlOwnerEvent struct {
	SchemaVersion int                       `json:"schema_version"`
	Stage         string                    `json:"stage"`
	Result        *controlOwnerResultRecord `json:"result,omitempty"`
}

type controlOwnerResultRecord struct {
	Started      bool                        `json:"started"`
	PID          int                         `json:"pid"`
	ExitCode     int                         `json:"exit_code"`
	StdoutBase64 string                      `json:"stdout_base64,omitempty"`
	StderrBase64 string                      `json:"stderr_base64,omitempty"`
	Snapshot     *controlOwnerSnapshotRecord `json:"snapshot,omitempty"`
	ErrorMessage string                      `json:"error_message,omitempty"`
	ErrorKind    string                      `json:"error_kind,omitempty"`
	Errno        int                         `json:"errno,omitempty"`
	ControlLimit bool                        `json:"control_limit,omitempty"`
}

type controlOwnerSnapshotRecord struct {
	Jobs   []controlOwnerJobRecord            `json:"jobs"`
	Groups map[string]controlOwnerGroupRecord `json:"groups"`
}

type controlOwnerJobRecord struct {
	ID              int64   `json:"id"`
	Label           *string `json:"label,omitempty"`
	State           State   `json:"state"`
	Group           string  `json:"group"`
	Succeeded       bool    `json:"succeeded"`
	OriginalCommand string  `json:"original_command,omitempty"`
}

type controlOwnerGroupRecord struct {
	Status        string `json:"status"`
	ParallelTasks uint64 `json:"parallel_tasks"`
}

type controlExitError struct {
	code    int
	message string
}

func (e *controlExitError) Error() string {
	if e == nil || e.message == "" {
		return "supervisor control process exited unsuccessfully"
	}
	return e.message
}

func (e *controlExitError) ExitCode() int {
	if e == nil {
		return -1
	}
	return e.code
}

func (e *controlExitError) Exited() bool { return e != nil }

// RunControlOwnerMode executes the hidden one-shot owner used by delegate-run.
// It starts one already-authorized supervisor command, owns its sole Wait, and
// carries no task, migration, retry, or publication policy.
func RunControlOwnerMode(args []string) (int, bool) {
	if len(args) == 0 || args[0] != controlOwnerMode {
		return 0, false
	}
	if len(args) != 4 {
		return 2, true
	}
	socketFD, socketErr := strconv.Atoi(args[1])
	leaseCount, leaseErr := strconv.Atoi(args[2])
	if socketErr != nil || leaseErr != nil || socketFD != controlOwnerSocketFD || leaseCount < 0 || leaseCount > 16 || task.ValidateTaskID(args[3]) != nil {
		return 2, true
	}
	if err := runControlOwner(socketFD, leaseCount, args[3]); err != nil {
		return 1, true
	}
	return 0, true
}

func runControlOwner(socketFD, leaseCount int, expectedCommandID string) (resultErr error) {
	connection, err := controlOwnerConnection(socketFD)
	if err != nil {
		return err
	}
	defer closeControlOwnerConnection(connection)
	request, err := readControlFrame[controlOwnerRequest](connection, maxControlOwnerRequestBytes)
	if err != nil {
		return err
	}
	reportFailure := func(ownerErr error) error {
		return writeControlOwnerEvent(connection, controlOwnerEvent{
			SchemaVersion: task.SchemaVersion,
			Stage:         "completed",
			Result: commandResultRecord(CommandResult{
				ExitCode: -1,
				Err:      ownerErr,
			}),
		})
	}
	if err = validateControlOwnerRequest(request); err != nil {
		return reportFailure(err)
	}
	if request.CommandID != expectedCommandID {
		return reportFailure(task.ErrIdentityMismatch)
	}
	leases, err := inheritedControlLeaseFiles(socketFD+1, leaseCount)
	if err != nil {
		return reportFailure(err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeControlLeaseFiles(leases))
	}()

	forwarded, forwardErr := controlOwnerShutdownFiles(request, leases)
	if forwardErr != nil {
		return reportFailure(forwardErr)
	}
	command := exec.Command(request.Executable, request.Arguments[1:]...)
	command.Args = append([]string(nil), request.Arguments...)
	command.Dir = request.Directory
	command.Env = os.Environ()
	command.ExtraFiles = forwarded
	deliveryAvailable := true
	result := runOwnedCommand(command, request.StatusVersion, func(started CommandResult) {
		event := controlOwnerEvent{
			SchemaVersion: task.SchemaVersion,
			Stage:         "started",
			Result:        commandResultRecord(started),
		}
		if writeErr := writeControlOwnerEvent(connection, event); writeErr != nil {
			deliveryAvailable = false
			closeControlOwnerConnection(connection)
		}
	})
	// Completion ownership does not depend on the observer remaining connected.
	// A caller can exit after its finite observation window; the owner must still
	// reap the child and release inherited leases naturally. Once Start occurs,
	// socket delivery is observational: the caller detects loss independently,
	// while owner exit success remains proof that Wait and lease release ran.
	if deliveryAvailable {
		if writeErr := writeControlOwnerCompletion(connection, result); writeErr != nil {
			closeControlOwnerConnection(connection)
		}
	}
	return nil
}

// closeControlOwnerConnection treats transport teardown as observer state.
// Command Wait and inherited lease release remain the owner's authority.
func closeControlOwnerConnection(connection net.Conn) {
	if connection == nil {
		return
	}
	if closeErr := connection.Close(); closeErr != nil {
		return
	}
}

func writeControlOwnerCompletion(connection net.Conn, result CommandResult) error {
	event := controlOwnerEvent{
		SchemaVersion: task.SchemaVersion,
		Stage:         "completed",
		Result:        commandResultRecord(result),
	}
	err := writeControlOwnerEvent(connection, event)
	if !errors.Is(err, task.ErrControlRecordTooBig) {
		return err
	}
	// Preserve a bounded operational result even when a projected queue
	// snapshot itself exceeds the IPC envelope.
	limited := CommandResult{
		Started:  result.Started,
		PID:      result.PID,
		ExitCode: result.ExitCode,
		Err:      errors.Join(result.Err, ErrControlLimit),
	}
	event.Result = commandResultRecord(limited)
	return writeControlOwnerEvent(connection, event)
}

// The caller owns the permit policy. This boundary prepares actual inherited
// resources before consuming it, then attempts the external owner's Start once.
func startControlOwnedCapturedWithConsume(ctx context.Context, ownerExecutable string, cmd *exec.Cmd, commandID string, observer func(CommandEvent), statusVersion string, consume func() error, report func(error)) *Pending {
	return startControlOwnedCapturedWithSettlement(ctx, ownerExecutable, cmd, commandID, observer, statusVersion, consume, report, nil)
}

// Written only by this command's canonical Start/Wait owner and read only
// after Pending.Done. No observed PID/Started bit grants settlement proof.
type controlCommandSettlement struct {
	pending          *Pending
	ownerStartFailed bool
	ownerStarted     bool
	commandSettled   bool
}

func (proof *controlCommandSettlement) bind(pending *Pending) {
	if proof != nil {
		proof.pending = pending
	}
}

func (proof *controlCommandSettlement) failedOwnerStart() {
	if proof != nil {
		proof.ownerStartFailed = true
	}
}

func (proof *controlCommandSettlement) startedOwner() {
	if proof != nil {
		proof.ownerStarted = true
	}
}

// permitsOrdinaryFailure distinguishes command provenance from mere owner
// settlement. A completed Started=false/errno report may be a prerequisite
// failure; it never grants ordinary-command retry permission.
func (proof *controlCommandSettlement) permitsOrdinaryFailure(err error) bool {
	if proof == nil || proof.pending == nil {
		return false
	}
	select {
	case <-proof.pending.Done():
		completion := classifyCommandCompletion(err)
		if proof.ownerStartFailed && !proof.ownerStarted {
			return completion.kind == commandCompletionErrno
		}
		result, complete := proof.pending.Result()
		return complete && proof.ownerStarted && proof.commandSettled && result.Started && result.ExitCode > 0 && completion.kind == commandCompletionExit
	default:
		return false
	}
}

func startControlOwnedCapturedWithSettlement(ctx context.Context, ownerExecutable string, cmd *exec.Cmd, commandID string, observer func(CommandEvent), statusVersion string, consume func() error, report func(error), settlement *controlCommandSettlement) *Pending {
	var commandArgs []string
	if cmd != nil {
		commandArgs = append([]string(nil), cmd.Args...)
	}
	pending := transport.NewPending(commandArgs, report)
	settlement.bind(pending)
	fail := func(err error) *Pending {
		result := CommandResult{ExitCode: -1, Err: err}
		if cmd != nil {
			transport.EmitCommand(observer, commandID, "completed", cmd, result)
		}
		transport.CompletePending(pending, result)
		return pending
	}
	if err := validateControlOwnerStart(ctx, ownerExecutable, cmd); err != nil {
		return fail(err)
	}
	transport.EmitCommand(observer, commandID, "entry", cmd, CommandResult{ExitCode: -1})
	duplicates, err := duplicateControlLeaseFiles(ctx)
	if err != nil {
		return fail(err)
	}
	leases := duplicates
	shutdown, shutdownErr := privateShutdownForwarding(ctx, duplicates)
	if shutdownErr != nil {
		return fail(errors.Join(shutdownErr, closeControlLeaseFiles(leases)))
	}
	if len(leases) > 16 {
		return fail(errors.Join(ErrConfiguration, closeControlLeaseFiles(leases)))
	}
	parent, child, err := newControlOwnerSocketPair()
	if err != nil {
		return fail(errors.Join(err, closeControlLeaseFiles(leases)))
	}
	owner := exec.Command(ownerExecutable, controlOwnerMode, strconv.Itoa(controlOwnerSocketFD), strconv.Itoa(len(leases)), commandID)
	owner.Dir = cmd.Dir
	owner.Env = slices.Clone(cmd.Env)
	owner.ExtraFiles = append([]*os.File{child}, leases...)
	request := controlOwnerRequest{
		SchemaVersion: task.SchemaVersion,
		CommandID:     commandID,
		Executable:    cmd.Path,
		Arguments:     append([]string(nil), cmd.Args...),
		Directory:     cmd.Dir,
		StatusVersion: statusVersion,
		Shutdown:      shutdown,
	}
	if err = consumePreparedControlCommand(ctx, request, consume); err != nil {
		return fail(errors.Join(err, parent.Close(), child.Close(), closeControlLeaseFiles(leases)))
	}
	if err = owner.Start(); err != nil {
		settlement.failedOwnerStart()
		return fail(errors.Join(err, parent.Close(), child.Close(), closeControlLeaseFiles(leases)))
	}
	settlement.startedOwner()
	closeErr := errors.Join(child.Close(), closeControlLeaseFiles(leases))
	if err = parent.SetWriteDeadline(time.Now().Add(controlOwnerWriteTimeout)); err == nil {
		err = writeControlFrame(parent, request, maxControlOwnerRequestBytes)
	}
	if err != nil || closeErr != nil {
		parentCloseErr := parent.Close()
		go completeControlOwnerPending(owner, nil, pending, cmd, commandID, observer, errors.Join(err, closeErr, parentCloseErr), settlement)
		return pending
	}
	if err = parent.SetWriteDeadline(time.Time{}); err != nil {
		parentCloseErr := parent.Close()
		go completeControlOwnerPending(owner, nil, pending, cmd, commandID, observer, errors.Join(err, parentCloseErr), settlement)
		return pending
	}
	go observeControlOwner(parent, owner, pending, cmd, commandID, observer, settlement)
	return pending
}

func validateControlOwnerStart(ctx context.Context, ownerExecutable string, cmd *exec.Cmd) error {
	if ctx == nil || !validExternalControlCommand(ownerExecutable, cmd) {
		return ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if IsLegacyStateRunnerPath(filepath.Dir(filepath.Dir(ownerExecutable)), ownerExecutable) {
		return errors.Join(ErrConfiguration, errors.New("historical runner copy cannot own new control commands"))
	}
	selected, err := canonicalExecutable(ownerExecutable)
	if err != nil {
		return err
	}
	if IsLegacyStateRunnerPath(filepath.Dir(filepath.Dir(selected)), selected) {
		return errors.Join(ErrConfiguration, errors.New("historical runner copy cannot own new control commands"))
	}
	return nil
}

func validExternalControlCommand(ownerExecutable string, cmd *exec.Cmd) bool {
	if cmd == nil || !validControlOwnerPath(ownerExecutable) {
		return false
	}
	return validControlCommandIdentity(cmd) && validControlCommandState(cmd)
}

func validControlOwnerPath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value
}

func validControlCommandIdentity(cmd *exec.Cmd) bool {
	return validControlOwnerPath(cmd.Path) &&
		len(cmd.Args) > 0 && cmd.Args[0] == cmd.Path &&
		validControlOwnerPath(cmd.Dir)
}

func validControlCommandState(cmd *exec.Cmd) bool {
	// The external owner reconstructs a clean exec.Cmd from immutable fields;
	// process-control fields are neither transported nor inherited.
	return cmd.Stdin == nil && cmd.Stdout == nil && cmd.Stderr == nil &&
		len(cmd.ExtraFiles) == 0 && cmd.SysProcAttr == nil && cmd.Cancel == nil &&
		cmd.Process == nil && cmd.ProcessState == nil && cmd.Err == nil
}

func newControlOwnerSocketPair() (net.Conn, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	parentFile := os.NewFile(uintptr(fds[0]), "delegate-control-parent")
	childFile := os.NewFile(uintptr(fds[1]), "delegate-control-child")
	if parentFile == nil || childFile == nil {
		var closeErr error
		if parentFile != nil {
			closeErr = errors.Join(closeErr, parentFile.Close())
		} else {
			closeErr = errors.Join(closeErr, unix.Close(fds[0]))
		}
		if childFile != nil {
			closeErr = errors.Join(closeErr, childFile.Close())
		} else {
			closeErr = errors.Join(closeErr, unix.Close(fds[1]))
		}
		return nil, nil, errors.Join(os.ErrInvalid, closeErr)
	}
	connection, err := net.FileConn(parentFile)
	closeErr := parentFile.Close()
	if err != nil {
		return nil, nil, errors.Join(err, closeErr, childFile.Close())
	}
	if closeErr != nil {
		return nil, nil, errors.Join(closeErr, connection.Close(), childFile.Close())
	}
	return connection, childFile, nil
}

func observeControlOwner(connection net.Conn, owner *exec.Cmd, pending *Pending, command *exec.Cmd, commandID string, observer func(CommandEvent), settlement *controlCommandSettlement) {
	result := CommandResult{ExitCode: -1}
	var protocolErr error
	completed := false
	started := false
	for !completed {
		event, err := readControlFrame[controlOwnerEvent](connection, maxControlOwnerResponseBytes)
		if err != nil {
			protocolErr = err
			break
		}
		if event.SchemaVersion != task.SchemaVersion || event.Result == nil {
			protocolErr = ErrConfiguration
			break
		}
		recorded, err := commandResultFromRecord(event.Result)
		if err != nil {
			protocolErr = err
			break
		}
		switch event.Stage {
		case "started":
			if started || !recorded.Started || recorded.ExitCode != -1 {
				protocolErr = ErrConfiguration
				break
			}
			started = true
			result = recorded
			transport.EmitCommand(observer, commandID, "started", command, recorded)
		case "completed":
			if recorded.Started != started {
				protocolErr = ErrConfiguration
				break
			}
			result = recorded
			completed = true
		default:
			protocolErr = ErrConfiguration
		}
		if protocolErr != nil {
			break
		}
	}
	closeErr := connection.Close()
	completeControlOwnerPending(owner, &result, pending, command, commandID, observer, errors.Join(protocolErr, closeErr), settlement)
}

func completeControlOwnerPending(owner *exec.Cmd, observed *CommandResult, pending *Pending, command *exec.Cmd, commandID string, observer func(CommandEvent), protocolErr error, settlement *controlCommandSettlement) {
	result := CommandResult{ExitCode: -1}
	if observed != nil {
		result = *observed
	}
	waitErr := owner.Wait()
	if settlement != nil {
		// A lost frame or failed owner Wait is never a completed-command proof,
		// even when Started is false and the remaining cause is a pure errno.
		settlement.commandSettled = observed != nil && protocolErr == nil && waitErr == nil
	}
	if protocolErr != nil || observed == nil {
		result.Err = errors.Join(result.Err, protocolErr, waitErr)
	} else if waitErr != nil {
		result.Err = errors.Join(result.Err, errors.New("supervisor control owner exited unsuccessfully"), waitErr)
	}
	transport.EmitCommand(observer, commandID, "completed", command, result)
	transport.CompletePending(pending, result)
}

func controlOwnerConnection(fd int) (net.Conn, error) {
	file := os.NewFile(uintptr(fd), "delegate-supervisor-control")
	if file == nil {
		return nil, errors.New("supervisor control owner inherited no IPC descriptor")
	}
	connection, err := net.FileConn(file)
	closeErr := file.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, connection.Close())
	}
	return connection, nil
}

func inheritedControlLeaseFiles(firstFD, count int) ([]*os.File, error) {
	files := make([]*os.File, 0, count)
	for index := 0; index < count; index++ {
		file := os.NewFile(uintptr(firstFD+index), "delegate-control-lease")
		if file == nil {
			return nil, errors.Join(errors.New("supervisor control owner inherited an invalid lease"), closeControlLeaseFiles(files))
		}
		files = append(files, file)
		if err := markControlLeaseCloseOnExec(file); err != nil {
			return nil, errors.Join(err, closeControlLeaseFiles(files))
		}
	}
	return files, nil
}

func closeControlLeaseFiles(files []*os.File) error {
	var result error
	for _, file := range files {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	return result
}

func validateControlOwnerRequest(request controlOwnerRequest) error {
	if request.SchemaVersion != task.SchemaVersion {
		return task.ErrUnsupportedSchema
	}
	if err := task.ValidateTaskID(request.CommandID); err != nil {
		return err
	}
	if !filepath.IsAbs(request.Executable) || filepath.Clean(request.Executable) != request.Executable {
		return ErrConfiguration
	}
	if !filepath.IsAbs(request.Directory) || filepath.Clean(request.Directory) != request.Directory {
		return ErrConfiguration
	}
	if len(request.Arguments) == 0 || request.Arguments[0] != request.Executable || len(request.Arguments) > 1024 {
		return ErrConfiguration
	}
	total := 0
	for _, argument := range request.Arguments {
		if strings.IndexByte(argument, 0) >= 0 {
			return ErrConfiguration
		}
		total += len(argument)
		if total > maxControlOwnerRequestBytes/2 {
			return ErrConfiguration
		}
	}
	if len(request.StatusVersion) > 1024 || strings.IndexByte(request.StatusVersion, 0) >= 0 {
		return ErrConfiguration
	}
	return validateControlOwnerShutdown(request)
}

func writeControlOwnerEvent(connection net.Conn, event controlOwnerEvent) error {
	if connection == nil {
		return net.ErrClosed
	}
	if err := connection.SetWriteDeadline(time.Now().Add(controlOwnerWriteTimeout)); err != nil {
		return err
	}
	return writeControlFrame(connection, event, maxControlOwnerResponseBytes)
}

func writeControlFrame[T any](writer io.Writer, value T, limit int) error {
	data, err := task.MarshalCanonical(value)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > limit {
		return task.ErrControlRecordTooBig
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if err = writeControlBytes(writer, header[:]); err != nil {
		return err
	}
	return writeControlBytes(writer, data)
}

func writeControlBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readControlFrame[T any](reader io.Reader, limit int) (T, error) {
	var zero T
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return zero, err
	}
	size := int(binary.BigEndian.Uint32(header[:]))
	if size <= 0 || size > limit {
		return zero, task.ErrControlRecordTooBig
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return zero, err
	}
	if err := task.ValidateJSONStructure(data); err != nil {
		return zero, err
	}
	if err := task.DecodeStrict(data, &zero); err != nil {
		return zero, err
	}
	return zero, nil
}

func commandResultRecord(result CommandResult) *controlOwnerResultRecord {
	record := &controlOwnerResultRecord{
		Started:      result.Started,
		PID:          result.PID,
		ExitCode:     result.ExitCode,
		StdoutBase64: base64.StdEncoding.EncodeToString(result.Stdout),
		StderrBase64: base64.StdEncoding.EncodeToString(result.Stderr),
	}
	if snapshot, ok := result.Snapshot(); ok {
		copy := snapshotRecord(snapshot)
		record.Snapshot = &copy
	}
	if result.Err == nil {
		return record
	}
	record.ErrorMessage = result.Err.Error()
	record.ControlLimit = errors.Is(result.Err, ErrControlLimit)
	record.ErrorKind, record.Errno = commandCompletionRecordKind(result)
	return record
}

func commandResultFromRecord(record *controlOwnerResultRecord) (CommandResult, error) {
	if err := validateControlOwnerResultRecord(record); err != nil {
		return CommandResult{}, err
	}
	stdout, err := decodeControlOwnerOutput(record.StdoutBase64)
	if err != nil {
		return CommandResult{}, err
	}
	stderr, err := decodeControlOwnerOutput(record.StderrBase64)
	if err != nil {
		return CommandResult{}, err
	}
	result := CommandResult{
		Started:  record.Started,
		PID:      record.PID,
		ExitCode: record.ExitCode,
		Stdout:   stdout,
		Stderr:   stderr,
	}
	if record.Snapshot != nil {
		snapshot, snapshotErr := snapshotFromRecord(*record.Snapshot)
		if snapshotErr != nil {
			return CommandResult{}, snapshotErr
		}
		result.SetSnapshot(snapshot)
	}
	result.Err, err = controlOwnerErrorFromRecord(record)
	if err != nil {
		return CommandResult{}, err
	}
	return result, nil
}

func validateControlOwnerResultRecord(record *controlOwnerResultRecord) error {
	if record == nil || record.ExitCode < -1 {
		return ErrConfiguration
	}
	if record.Started {
		if record.PID <= 0 {
			return ErrConfiguration
		}
	} else if record.PID != 0 || record.ExitCode != -1 {
		return ErrConfiguration
	}
	return validateControlOwnerErrorFields(record)
}

func decodeControlOwnerOutput(encoded string) ([]byte, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) > MaxControlBytes {
		return nil, ErrConfiguration
	}
	return decoded, nil
}

func snapshotRecord(snapshot QueueSnapshot) controlOwnerSnapshotRecord {
	record := controlOwnerSnapshotRecord{
		Jobs:   make([]controlOwnerJobRecord, len(snapshot.Jobs)),
		Groups: make(map[string]controlOwnerGroupRecord, len(snapshot.Groups)),
	}
	for index, job := range snapshot.Jobs {
		record.Jobs[index] = controlOwnerJobRecord{
			ID: job.ID, Label: job.Label, State: job.State, Group: job.Group,
			Succeeded: job.Succeeded, OriginalCommand: originalCommand(job),
		}
	}
	for name, group := range snapshot.Groups {
		record.Groups[name] = controlOwnerGroupRecord(group)
	}
	return record
}

func snapshotFromRecord(record controlOwnerSnapshotRecord) (QueueSnapshot, error) {
	snapshot := QueueSnapshot{Jobs: make([]Job, len(record.Jobs)), Groups: make(map[string]Group, len(record.Groups))}
	for index, job := range record.Jobs {
		if job.ID < 0 || (job.State != StateUnknown && job.State != StateQueued && job.State != StateStashed && job.State != StateRunning && job.State != StateEnded) ||
			job.Group == "" || len(job.OriginalCommand) > MaxControlBytes || strings.IndexByte(job.OriginalCommand, 0) >= 0 {
			return QueueSnapshot{}, ErrConfiguration
		}
		if job.Label != nil {
			label := *job.Label
			job.Label = &label
		}
		snapshot.Jobs[index] = jobWithOriginalCommand(Job{
			ID: job.ID, Label: job.Label, State: job.State, Group: job.Group,
			Succeeded: job.Succeeded,
		}, job.OriginalCommand)
	}
	for name, group := range record.Groups {
		if name == "" || group.Status == "" {
			return QueueSnapshot{}, ErrConfiguration
		}
		snapshot.Groups[name] = Group(group)
	}
	return snapshot, nil
}

// ControlLease supplies inherited duplicates of an already-held lifecycle
// lease. The caller remains the authority that releases the original lease.
type ControlLease interface {
	DuplicateControlLeaseFiles() ([]*os.File, error)
}

type controlLeaseContextKey struct{}

type controlLeaseContext struct {
	locks []*bootstrapLock
	extra []ControlLease
}

// WithControlLease adds a caller-owned lease that must outlive a supervisor
// control mutation. The lease remains owned and released by its caller; the
// control owner receives only inherited duplicate descriptors.
func WithControlLease(ctx context.Context, lease ControlLease) context.Context {
	if ctx == nil || lease == nil {
		return ctx
	}
	current := controlLeaseContextFrom(ctx)
	next := &controlLeaseContext{locks: append([]*bootstrapLock(nil), current.locks...), extra: append([]ControlLease(nil), current.extra...)}
	next.extra = append(next.extra, lease)
	return context.WithValue(ctx, controlLeaseContextKey{}, next)
}

// withControlLockLease appends one already-held supervisor lock to the set an
// external command must inherit. Nested scopes compose; an inner queue lock
// must never replace an outer supervisor-generation lease.
func withControlLockLease(ctx context.Context, lock *bootstrapLock) context.Context {
	if ctx == nil || lock == nil {
		return ctx
	}
	current := controlLeaseContextFrom(ctx)
	next := &controlLeaseContext{locks: append([]*bootstrapLock(nil), current.locks...), extra: append([]ControlLease(nil), current.extra...)}
	for _, currentLock := range next.locks {
		if currentLock == lock {
			return ctx
		}
	}
	next.locks = append(next.locks, lock)
	return context.WithValue(ctx, controlLeaseContextKey{}, next)
}

func controlLeaseContextFrom(ctx context.Context) *controlLeaseContext {
	if ctx == nil {
		return &controlLeaseContext{}
	}
	value, ok := ctx.Value(controlLeaseContextKey{}).(*controlLeaseContext)
	if !ok || value == nil {
		return &controlLeaseContext{}
	}
	return value
}

func duplicateControlLeaseFiles(ctx context.Context) ([]*os.File, error) {
	leases := controlLeaseContextFrom(ctx)
	files := make([]*os.File, 0, len(leases.locks)+len(leases.extra)*2)
	for _, lock := range leases.locks {
		file, err := lock.duplicateForControlOwner()
		if err != nil {
			return nil, errors.Join(err, closeControlLeaseFiles(files))
		}
		files = append(files, file)
	}
	for _, lease := range leases.extra {
		duplicates, err := lease.DuplicateControlLeaseFiles()
		if err != nil {
			return nil, errors.Join(err, closeControlLeaseFiles(files))
		}
		files = append(files, duplicates...)
	}
	return files, nil
}

func duplicateCloseOnExec(file *os.File, name string) (*os.File, error) {
	if file == nil {
		return nil, os.ErrInvalid
	}
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	duplicate := os.NewFile(uintptr(fd), name)
	if duplicate == nil {
		return nil, unix.Close(fd)
	}
	return duplicate, nil
}
