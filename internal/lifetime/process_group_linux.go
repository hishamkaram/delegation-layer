//go:build linux

package lifetime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	processSignalTerm            = syscall.SIGTERM
	processSignalKill            = syscall.SIGKILL
	errProcessNoSuchEntity error = unix.ESRCH
)

func isLinuxProcessAbsentError(err error) bool {
	return errors.Is(err, unix.ESRCH) || errors.Is(err, os.ErrNotExist)
}

func configureProcessGroupCommand(_ *processGroupScope, cmd *exec.Cmd) error {
	// A private session keeps the caller outside the cooperative ownership
	// closure and lets discovery retain descendants that create new groups or
	// sessions when delegated cgroups are unavailable.
	return ConfigureIndependentSession(cmd)
}

func configureProcessGroupOwner() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("configure Linux process-scope owner: %w", err)
	}
	return nil
}

func observeProcessIdentity(pid int) (bool, string, error) {
	present, zombie, token, err := inspectLinuxProcessIdentity(pid)
	if err != nil || !present || zombie {
		return present && !zombie, "", err
	}
	return true, token, nil
}

// observeStartedProcessIdentity accepts an unreaped zombie because cmd.Start
// succeeded and the caller still owns the single Wait. Until Wait reaps it,
// the PID cannot be reused and its start token remains stable launch evidence.
func observeStartedProcessIdentity(pid int) (bool, string, error) {
	present, _, token, err := inspectLinuxProcessIdentity(pid)
	if err != nil || !present {
		return present, "", err
	}
	return true, token, nil
}

func inspectLinuxProcessIdentity(pid int) (bool, bool, string, error) {
	if pid <= 0 {
		return false, false, "", fmt.Errorf("%w: invalid PID %d", ErrInvalidLaunchRef, pid)
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if isLinuxProcessAbsentError(err) {
		return false, false, "", nil
	}
	if err != nil {
		return false, false, "", fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	return parseLinuxProcessStatRecord(data)
}

func parseLinuxProcessStat(data []byte) (bool, string, error) {
	present, zombie, token, err := parseLinuxProcessStatRecord(data)
	if err != nil || !present || zombie {
		return present && !zombie, "", err
	}
	return true, token, nil
}

func parseLinuxProcessStatRecord(data []byte) (bool, bool, string, error) {
	record, err := parseLinuxProcessScopeStat(data)
	if err != nil {
		return false, false, "", err
	}
	return true, record.zombie, record.snapshot.startToken, nil
}

type linuxProcessScopeStat struct {
	snapshot processScopeSnapshot
	zombie   bool
}

func parseLinuxProcessScopeStat(data []byte) (linuxProcessScopeStat, error) {
	text := strings.TrimSpace(string(data))
	opening := strings.Index(text, " (")
	closing := strings.LastIndex(text, ")")
	if opening <= 0 || closing <= opening+1 || closing+2 > len(text) || text[closing+1] != ' ' {
		return linuxProcessScopeStat{}, errors.New("linux process stat is malformed")
	}
	pid, err := parseLinuxProcessStatInteger(text[:opening], "PID")
	if err != nil || pid <= 1 {
		return linuxProcessScopeStat{}, errors.New("linux process stat PID is invalid")
	}
	fields := strings.Fields(text[closing+2:])
	// The suffix starts at proc(5)'s field 3 (state). PPID, process group,
	// session, and starttime are indexes 1, 2, 3, and 19 in this suffix.
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return linuxProcessScopeStat{}, errors.New("linux process stat is incomplete")
	}
	parentPID, pgid, sessionID, err := parseLinuxProcessScopeTopology(fields)
	if err != nil {
		return linuxProcessScopeStat{}, err
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || strconv.FormatUint(start, 10) != fields[19] {
		return linuxProcessScopeStat{}, errors.New("linux process start token is invalid")
	}
	return linuxProcessScopeStat{
		snapshot: processScopeSnapshot{
			pid:        pid,
			parentPID:  parentPID,
			pgid:       pgid,
			sessionID:  sessionID,
			startToken: "linux:" + fields[19],
			zombie:     fields[0] == "Z",
		},
		zombie: fields[0] == "Z",
	}, nil
}

func parseLinuxProcessScopeTopology(fields []string) (int, int, int, error) {
	parentPID, err := parseLinuxProcessStatInteger(fields[1], "PPID")
	if err != nil || parentPID < 0 {
		return 0, 0, 0, errors.New("linux process stat PPID is invalid")
	}
	pgid, err := parseLinuxProcessStatInteger(fields[2], "process group")
	if err != nil {
		return 0, 0, 0, errors.New("linux process stat process group is invalid")
	}
	sessionID, err := parseLinuxProcessStatInteger(fields[3], "session")
	if err != nil {
		return 0, 0, 0, errors.New("linux process stat session is invalid")
	}
	return parentPID, pgid, sessionID, nil
}

func parseLinuxProcessStatInteger(value, field string) (int, error) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || strconv.FormatInt(parsed, 10) != value {
		return 0, fmt.Errorf("linux process stat %s is malformed", field)
	}
	return int(parsed), nil
}

func processGroupID(pid int) (int, error) {
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return 0, err
	}
	return pgid, nil
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	return rawProcessSignal(-pgid, signal)
}

type linuxProcessHandle struct {
	pid int
	fd  int
}

type linuxPidfdOperations struct {
	open    func(int) (int, error)
	inspect func(int) (linuxProcessScopeStat, bool, error)
	close   func(int) error
}

func nativeLinuxPidfdOperations() linuxPidfdOperations {
	return linuxPidfdOperations{
		open: func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) },
		inspect: func(pid int) (linuxProcessScopeStat, bool, error) {
			data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if isLinuxProcessAbsentError(err) {
				return linuxProcessScopeStat{}, false, nil
			}
			if err != nil {
				return linuxProcessScopeStat{}, false, err
			}
			record, err := parseLinuxProcessScopeStat(data)
			if err != nil {
				return linuxProcessScopeStat{}, false, err
			}
			return record, !record.zombie, nil
		},
		close: unix.Close,
	}
}

func signalVerifiedProcessGroup(ctx context.Context, _ *processGroupScope, pgid int, refs []LaunchRef, signal syscall.Signal) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	handles, err := openVerifiedLinuxProcessHandles(pgid, refs, nativeLinuxPidfdOperations())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeLinuxProcessHandles(handles, unix.Close)) }()
	if len(handles) == 0 {
		return errProcessNoSuchEntity
	}
	for _, handle := range handles {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = unix.PidfdSendSignal(handle.fd, unix.Signal(signal), nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("signal PID %d through pidfd: %w", handle.pid, err)
		}
	}
	return nil
}

func validateProcessGroupReferenceIdentifier(identifier string) error {
	return validateProcessGroupPlanIdentifier(identifier)
}

func openProcessGroupPlatformState(ctx context.Context, ref Ref) (string, processGroupPin, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	return ref.identifier, nil, nil
}

func bindProcessGroupPlatformState(*processGroupScope) error { return nil }

func processGroupPlatformReference(scope *processGroupScope) Ref {
	return Ref{kind: KindProcessGroup, assurance: AssuranceCooperative, identifier: scope.identifier}
}

func validateProcessGroupLaunch(_ *processGroupScope, pid, pgid int) error {
	if pgid != pid {
		return fmt.Errorf("%w: PID %d is in PGID %d, want its own group", ErrUnverifiableLaunch, pid, pgid)
	}
	return nil
}

func validateProcessGroupSignalTargets(_ *processGroupScope, _ map[int][]LaunchRef) error { return nil }

func processGroupPlatformInfrastructure(*processGroupScope) []ProcessRef { return nil }

func releaseProcessGroupPlatformState(*processGroupScope) error { return nil }

func destroyProcessGroupPlatformState(*processGroupScope) error { return nil }

func openVerifiedLinuxProcessHandles(pgid int, refs []LaunchRef, operations linuxPidfdOperations) ([]linuxProcessHandle, error) {
	if len(refs) == 0 || operations.open == nil || operations.inspect == nil || operations.close == nil {
		return nil, fmt.Errorf("%w: no stable process-handle verifier for group %d", ErrUnverifiableLaunch, pgid)
	}
	handles := make([]linuxProcessHandle, 0, len(refs))
	seen := make(map[int]struct{}, len(refs))
	for _, ref := range refs {
		if err := validateLaunchRefShape(ref); err != nil || ref.pgid != pgid {
			return nil, errors.Join(
				fmt.Errorf("%w: launch reference does not belong to process group %d", ErrUnverifiableLaunch, pgid),
				closeLinuxProcessHandles(handles, operations.close),
			)
		}
		if _, exists := seen[ref.pid]; exists {
			continue
		}
		handle, present, err := openVerifiedLinuxProcessHandle(pgid, ref, operations)
		if err != nil {
			return nil, errors.Join(err, closeLinuxProcessHandles(handles, operations.close))
		}
		if !present {
			continue
		}
		seen[ref.pid] = struct{}{}
		handles = append(handles, handle)
	}
	return handles, nil
}

func openVerifiedLinuxProcessHandle(pgid int, ref LaunchRef, operations linuxPidfdOperations) (linuxProcessHandle, bool, error) {
	fd, err := operations.open(ref.pid)
	if isLinuxProcessAbsentError(err) {
		return linuxProcessHandle{}, false, nil
	}
	if err != nil {
		record, present, inspectErr := operations.inspect(ref.pid)
		if inspectErr == nil && (!present || record.snapshot.startToken != ref.startToken || record.snapshot.pgid != pgid) {
			return linuxProcessHandle{}, false, nil
		}
		return linuxProcessHandle{}, false, fmt.Errorf("open pidfd for PID %d: %w", ref.pid, err)
	}
	record, present, inspectErr := operations.inspect(ref.pid)
	if inspectErr == nil && present && record.snapshot.startToken == ref.startToken && record.snapshot.pgid == pgid {
		return linuxProcessHandle{pid: ref.pid, fd: fd}, true, nil
	}
	closeErr := operations.close(fd)
	if inspectErr != nil || closeErr != nil {
		return linuxProcessHandle{}, false, errors.Join(inspectErr, closeErr)
	}
	return linuxProcessHandle{}, false, nil
}

func processGroupMembersPresent(pgid int, refs []LaunchRef) (bool, error) {
	return processGroupMembersPresentWith(pgid, refs, observeProcessIdentity, processGroupID)
}

func closeLinuxProcessHandles(handles []linuxProcessHandle, closeHandle func(int) error) error {
	if closeHandle == nil {
		return fmt.Errorf("%w: pidfd closer is unavailable", ErrUnverifiableLaunch)
	}
	var resultErr error
	for _, handle := range handles {
		resultErr = errors.Join(resultErr, closeHandle(handle.fd))
	}
	return resultErr
}

func requireLinuxPidfd() error {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf("%w: Linux pidfd support is unavailable: %w", ErrUnsupported, err)
	}
	if err = unix.Close(fd); err != nil {
		return fmt.Errorf("close Linux pidfd capability probe: %w", err)
	}
	return nil
}

func rawProcessSignal(pid int, signal syscall.Signal) error {
	return unix.Kill(pid, signal)
}

func processGroupExists(pgid int) (bool, error) {
	err := rawProcessSignal(-pgid, 0)
	if err == nil || errors.Is(err, unix.EPERM) {
		return true, nil
	}
	if isLinuxProcessAbsentError(err) {
		return false, nil
	}
	return false, err
}
