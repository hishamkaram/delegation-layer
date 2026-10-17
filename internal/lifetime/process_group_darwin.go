//go:build darwin

package lifetime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

var (
	processSignalTerm            = syscall.SIGTERM
	processSignalKill            = syscall.SIGKILL
	errProcessNoSuchEntity error = unix.ESRCH

	// libSystem owns the process-info ABI. Resolve its wrapper once so every
	// identity read and audit-token signal uses the same immutable binding.
	darwinProcInfoSymbol, darwinProcInfoSymbolErr = purego.Dlsym(purego.RTLD_DEFAULT, "__proc_info")
)

const (
	darwinProcInfoCallPIDInfo                int32  = 2
	darwinProcInfoCallSignalAuditToken       int32  = 0x11
	darwinProcPIDUniqIdentifierInfo          uint32 = 17
	darwinProcUniqIdentifierInfoSize                = 56
	darwinAuditTokenWords                           = 8
	darwinAuditTokenSize                            = darwinAuditTokenWords * 4
	darwinProcessIdentityTokenVersion               = "darwin-v2"
	darwinProcessIdentityObservationAttempts        = 3
	darwinProcessIdentityRefreshAttempts            = 3
)

type darwinProcInfoCall func(callNum int32, pid int32, flavor uint32, arg uint64, buffer []byte) (int, error)

type darwinProcessKinfoLookup func(pid int) (*unix.KinfoProc, error)

type darwinProcessIdentity struct {
	startToken string
	uniqueID   uint64
	pidVersion int32
	zombie     bool
}

type darwinAuditSignal func(pid int, pidVersion int32, signal syscall.Signal) error

func configureProcessGroupOwner() error {
	return nil
}

func configureProcessGroupCommand(scope *processGroupScope, cmd *exec.Cmd) error {
	attr, err := validatedDarwinProcessAttributes(cmd)
	if err != nil {
		return err
	}
	if scope == nil || scope.pin == nil {
		// Each new provider launch owns a private session. Discovery retains the
		// marker and exact topology ledger so children that create nested sessions
		// remain discoverable without a helper process or numeric group authority.
		return ConfigureIndependentSession(cmd)
	}
	return configurePinnedDarwinProcessGroupCommand(cmd, attr, scope.pin.processRef())
}

func validatedDarwinProcessAttributes(cmd *exec.Cmd) (syscall.SysProcAttr, error) {
	if cmd == nil {
		return syscall.SysProcAttr{}, fmt.Errorf("%w: nil command", ErrInvalidRef)
	}
	if cmd.Process != nil || cmd.ProcessState != nil {
		return syscall.SysProcAttr{}, fmt.Errorf("%w: command has already started", ErrConflictingSysProc)
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
		if attr.Setctty || attr.Foreground || attr.Ctty != 0 {
			return syscall.SysProcAttr{}, fmt.Errorf("%w: process-scope launch conflicts with terminal or PGID settings", ErrConflictingSysProc)
		}
	}
	return attr, nil
}

func configurePinnedDarwinProcessGroupCommand(cmd *exec.Cmd, attr syscall.SysProcAttr, pin ProcessRef) error {
	if err := validateProcessRef(pin); err != nil {
		return err
	}
	if attr.Setsid {
		return fmt.Errorf("%w: legacy Darwin process-group pin conflicts with session settings", ErrConflictingSysProc)
	}
	if attr.Setpgid || attr.Pgid != 0 {
		if attr.Setpgid && attr.Pgid == pin.PID() {
			return nil
		}
		return fmt.Errorf("%w: process-scope launch conflicts with terminal or PGID settings", ErrConflictingSysProc)
	}
	if pin.PID() <= 1 {
		return fmt.Errorf("%w: invalid Darwin process-group pin", ErrUnverifiableLaunch)
	}
	// Existing pinned references retain their historical launch topology until
	// their immutable records drain.
	attr.Setpgid = true
	attr.Pgid = pin.PID()
	cmd.SysProcAttr = &attr
	return nil
}

func observeProcessIdentity(pid int) (bool, string, error) {
	present, zombie, token, err := inspectDarwinProcessIdentity(pid)
	if err != nil || !present || zombie {
		return present && !zombie, "", err
	}
	return true, token, nil
}

// observeStartedProcessIdentity accepts an unreaped zombie because cmd.Start
// succeeded and the caller still owns the single Wait. Until Wait reaps it,
// the PID cannot be reused and its start token remains stable launch evidence.
func observeStartedProcessIdentity(pid int) (bool, string, error) {
	present, _, token, err := inspectDarwinProcessIdentity(pid)
	if err != nil || !present {
		return present, "", err
	}
	return true, token, nil
}

func inspectDarwinProcessIdentity(pid int) (bool, bool, string, error) {
	present, identity, err := inspectDarwinProcessIdentityWith(pid, nativeDarwinProcInfo, nativeDarwinProcessKinfoLookup)
	if err != nil || !present {
		return present, false, "", err
	}
	return true, identity.zombie, identity.startToken, nil
}

func inspectDarwinProcessIdentityInfo(pid int) (bool, darwinProcessIdentity, error) {
	return inspectDarwinProcessIdentityWith(pid, nativeDarwinProcInfo, nativeDarwinProcessKinfoLookup)
}

func nativeDarwinProcessKinfoLookup(pid int) (*unix.KinfoProc, error) {
	raw, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	return decodeDarwinKinfoProc(raw)
}

func decodeDarwinKinfoProc(raw []byte) (*unix.KinfoProc, error) {
	if len(raw) == 0 {
		// kern.proc.pid reports a successful zero-byte result when the PID
		// exits during or before the read. SysctlKinfoProc turns this into
		// EIO, losing the distinction between absence and an ABI mismatch.
		return nil, nil
	}
	if len(raw) != unix.SizeofKinfoProc {
		return nil, fmt.Errorf(
			"%w: Darwin kinfo response size %d, want %d",
			ErrUnverifiableLaunch,
			len(raw),
			unix.SizeofKinfoProc,
		)
	}
	var process unix.KinfoProc
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&process)), unix.SizeofKinfoProc), raw)
	return &process, nil
}

func nativeDarwinProcInfo(callNum int32, pid int32, flavor uint32, arg uint64, buffer []byte) (int, error) {
	if len(buffer) > int(^uint32(0)>>1) {
		return -1, fmt.Errorf("%w: Darwin proc_info buffer is too large", ErrInvalidLaunchRef)
	}
	if darwinProcInfoSymbolErr != nil {
		return -1, fmt.Errorf("%w: Darwin libSystem proc_info wrapper is unavailable: %w", ErrUnverifiableLaunch, darwinProcInfoSymbolErr)
	}
	if darwinProcInfoSymbol == 0 {
		return -1, fmt.Errorf("%w: Darwin libSystem proc_info wrapper has no address", ErrUnverifiableLaunch)
	}
	var bufferPointer uintptr
	if len(buffer) > 0 {
		bufferPointer = uintptr(unsafe.Pointer(&buffer[0]))
	}
	result, _, errno := purego.SyscallN(
		darwinProcInfoSymbol,
		uintptr(callNum),
		uintptr(pid),
		uintptr(flavor),
		uintptr(arg),
		bufferPointer,
		uintptr(len(buffer)),
	)
	returned := int(int32(result))
	if returned != -1 {
		return returned, nil
	}
	nativeErrno := darwinProcInfoErrno(errno)
	if nativeErrno == 0 {
		return -1, fmt.Errorf("%w: Darwin libSystem proc_info failed without errno", ErrUnverifiableLaunch)
	}
	return -1, nativeErrno
}

// purego stores Darwin's int errno in the low word of a uintptr-sized return
// slot. The upper word is unspecified and must not become part of Errno.
func darwinProcInfoErrno(value uintptr) syscall.Errno {
	return syscall.Errno(uint32(value))
}

func inspectDarwinProcessIdentityWith(
	pid int,
	procInfo darwinProcInfoCall,
	lookup darwinProcessKinfoLookup,
) (bool, darwinProcessIdentity, error) {
	if pid <= 0 {
		return false, darwinProcessIdentity{}, fmt.Errorf("%w: invalid PID %d", ErrInvalidLaunchRef, pid)
	}
	if procInfo == nil || lookup == nil {
		return false, darwinProcessIdentity{}, fmt.Errorf("%w: Darwin process identity ABI is unavailable", ErrUnverifiableLaunch)
	}
	var transitionErr error
	for attempt := 0; attempt < darwinProcessIdentityObservationAttempts; attempt++ {
		present, identity, retry, err := inspectDarwinProcessIdentityOnce(pid, procInfo, lookup)
		if !retry {
			return present, identity, err
		}
		transitionErr = err
	}
	return false, darwinProcessIdentity{}, transitionErr
}

func inspectDarwinProcessIdentityOnce(
	pid int,
	procInfo darwinProcInfoCall,
	lookup darwinProcessKinfoLookup,
) (bool, darwinProcessIdentity, bool, error) {
	process, err := lookup(pid)
	if err != nil {
		return classifyDarwinProcessLookupError(pid, err)
	}
	if process == nil {
		return false, darwinProcessIdentity{}, false, nil
	}
	if process.Proc.P_pid != int32(pid) {
		return false, darwinProcessIdentity{}, false, errors.New("darwin process identity is ambiguous")
	}
	identity, err := darwinProcessIdentityFromKinfo(pid, *process, procInfo)
	if err != nil {
		return classifyDarwinProcessInfoError(pid, err)
	}
	return true, identity, false, nil
}

func classifyDarwinProcessLookupError(pid int, err error) (bool, darwinProcessIdentity, bool, error) {
	if errors.Is(err, unix.ESRCH) {
		return false, darwinProcessIdentity{}, false, nil
	}
	resultErr := fmt.Errorf("inspect PID %d: %w", pid, err)
	return false, darwinProcessIdentity{}, errors.Is(err, unix.EIO), resultErr
}

func classifyDarwinProcessInfoError(pid int, err error) (bool, darwinProcessIdentity, bool, error) {
	if errors.Is(err, unix.ESRCH) {
		return false, darwinProcessIdentity{}, false, nil
	}
	if errors.Is(err, unix.EIO) {
		return false, darwinProcessIdentity{}, true, fmt.Errorf("inspect PID %d identity: %w", pid, err)
	}
	return false, darwinProcessIdentity{}, false, err
}

func darwinProcessIdentityFromKinfo(pid int, process unix.KinfoProc, procInfo darwinProcInfoCall) (darwinProcessIdentity, error) {
	if process.Proc.P_pid != int32(pid) {
		return darwinProcessIdentity{}, errors.New("darwin process identity PID does not match lookup")
	}
	start := process.Proc.P_starttime
	if start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1_000_000 {
		return darwinProcessIdentity{}, fmt.Errorf("%w: invalid Darwin start identity for PID %d", ErrUnverifiableLaunch, pid)
	}
	uniqueID, pidVersion, err := readDarwinUniqueIdentifierInfo(pid, procInfo)
	if err != nil {
		return darwinProcessIdentity{}, err
	}
	return darwinProcessIdentity{
		startToken: darwinProcessLaunchToken(start, uniqueID),
		uniqueID:   uniqueID,
		pidVersion: pidVersion,
		zombie:     process.Proc.P_stat == 5,
	}, nil
}

func readDarwinUniqueIdentifierInfo(pid int, procInfo darwinProcInfoCall) (uint64, int32, error) {
	if pid <= 0 {
		return 0, 0, fmt.Errorf("%w: invalid Darwin process PID %d", ErrInvalidLaunchRef, pid)
	}
	if procInfo == nil {
		return 0, 0, fmt.Errorf("%w: Darwin proc_info identity call is unavailable", ErrUnverifiableLaunch)
	}
	response := make([]byte, darwinProcUniqIdentifierInfoSize)
	returned, err := procInfo(
		darwinProcInfoCallPIDInfo,
		int32(pid),
		darwinProcPIDUniqIdentifierInfo,
		0,
		response,
	)
	if err != nil {
		return 0, 0, err
	}
	if returned != darwinProcUniqIdentifierInfoSize || len(response) != darwinProcUniqIdentifierInfoSize {
		return 0, 0, fmt.Errorf(
			"%w: Darwin unique-identifier response size %d, want %d",
			ErrUnverifiableLaunch,
			returned,
			darwinProcUniqIdentifierInfoSize,
		)
	}
	uniqueID := binary.LittleEndian.Uint64(response[16:24])
	if uniqueID == 0 {
		return 0, 0, fmt.Errorf("%w: Darwin PID %d has no unique process identity", ErrUnverifiableLaunch, pid)
	}
	pidVersion := int32(binary.LittleEndian.Uint32(response[32:36]))
	if pidVersion < 0 {
		return 0, 0, fmt.Errorf("%w: Darwin PID %d has invalid pidversion %d", ErrUnverifiableLaunch, pid, pidVersion)
	}
	return uniqueID, pidVersion, nil
}

func darwinProcessInfoCapability() error {
	present, identity, err := inspectDarwinProcessIdentityWith(os.Getpid(), nativeDarwinProcInfo, nativeDarwinProcessKinfoLookup)
	if err != nil {
		return fmt.Errorf("%w: Darwin proc_info identity capability: %w", ErrUnverifiableLaunch, err)
	}
	if !present || identity.uniqueID == 0 {
		return fmt.Errorf("%w: Darwin proc_info identity capability returned no current process", ErrUnverifiableLaunch)
	}
	return nil
}

func darwinAuditToken(pid int, pidVersion int32) ([]byte, error) {
	if pid <= 0 || pidVersion < 0 {
		return nil, fmt.Errorf("%w: invalid Darwin audit-token identity PID=%d pidversion=%d", ErrUnverifiableLaunch, pid, pidVersion)
	}
	token := make([]byte, darwinAuditTokenSize)
	binary.LittleEndian.PutUint32(token[5*4:6*4], uint32(pid))
	binary.LittleEndian.PutUint32(token[7*4:8*4], uint32(pidVersion))
	return token, nil
}

func signalDarwinProcessWith(
	ctx context.Context,
	ref LaunchRef,
	signal syscall.Signal,
	observe func(int) (bool, darwinProcessIdentity, error),
	send darwinAuditSignal,
) (bool, error) {
	if err := validateDarwinProcessSignalRequest(ctx, ref, signal, observe, send); err != nil {
		return false, err
	}
	for attempt := 0; attempt < darwinProcessIdentityRefreshAttempts; attempt++ {
		present, retry, err := signalDarwinProcessOnce(ctx, ref, signal, observe, send)
		if !retry {
			return present, err
		}
		// A Darwin exec can advance pidversion while preserving the stable
		// uniqueid. The next bounded pass re-reads it before retrying.
	}
	return false, errProcessNoSuchEntity
}

func validateDarwinProcessSignalRequest(
	ctx context.Context,
	ref LaunchRef,
	signal syscall.Signal,
	observe func(int) (bool, darwinProcessIdentity, error),
	send darwinAuditSignal,
) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := validateLaunchRefShape(ref); err != nil {
		return err
	}
	if signal != syscall.SIGTERM && signal != syscall.SIGKILL {
		return fmt.Errorf("%w: unsupported Darwin process signal %d", ErrInvalidRef, signal)
	}
	if observe == nil || send == nil {
		return fmt.Errorf("%w: Darwin audit-token signal path is unavailable", ErrUnverifiableLaunch)
	}
	return nil
}

func signalDarwinProcessOnce(
	ctx context.Context,
	ref LaunchRef,
	signal syscall.Signal,
	observe func(int) (bool, darwinProcessIdentity, error),
	send darwinAuditSignal,
) (bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	present, identity, err := observe(ref.PID())
	if errors.Is(err, unix.ESRCH) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !present || !processIdentityTokensMatch(identity.startToken, ref.StartToken()) {
		return false, false, nil
	}
	if err = send(ref.PID(), identity.pidVersion, signal); err == nil {
		return true, false, nil
	}
	return false, errors.Is(err, unix.ESRCH), err
}

func signalDarwinProcess(pid int, pidVersion int32, signal syscall.Signal) error {
	token, err := darwinAuditToken(pid, pidVersion)
	if err != nil {
		return err
	}
	if signal != syscall.SIGTERM && signal != syscall.SIGKILL {
		return fmt.Errorf("%w: unsupported Darwin process signal %d", ErrInvalidRef, signal)
	}
	returned, err := nativeDarwinProcInfo(
		darwinProcInfoCallSignalAuditToken,
		0,
		uint32(signal),
		0,
		token,
	)
	if err != nil {
		return err
	}
	if returned != 0 {
		return fmt.Errorf("%w: Darwin audit-token signal returned %d", ErrUnverifiableLaunch, returned)
	}
	return nil
}

func processGroupID(pid int) (int, error) {
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return 0, err
	}
	return pgid, nil
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	return fmt.Errorf("%w: numeric Darwin process-group signaling is unavailable", ErrUnsupported)
}

func verifyLaunchIdentities(pgid int, refs []LaunchRef) error {
	return verifyLaunchIdentitiesWith(pgid, refs, observeProcessIdentity, processGroupID)
}

func signalVerifiedProcessGroup(ctx context.Context, scope *processGroupScope, pgid int, refs []LaunchRef, signal syscall.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope != nil && scope.pin == nil {
		return signalDarwinProcessGroupWith(
			ctx,
			pgid,
			refs,
			signal,
			inspectDarwinProcessIdentityInfo,
			signalDarwinProcess,
		)
	}
	if err := verifyLaunchIdentities(pgid, refs); err != nil {
		return err
	}
	return signalDarwinPinnedProcessGroup(ctx, scope, pgid, signal)
}

func signalDarwinProcessGroupWith(
	ctx context.Context,
	pgid int,
	refs []LaunchRef,
	signal syscall.Signal,
	observe func(int) (bool, darwinProcessIdentity, error),
	send darwinAuditSignal,
) error {
	if ctx == nil {
		return ErrNilContext
	}
	signaled := false
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateLaunchRefShape(ref); err != nil || ref.PGID() != pgid {
			return fmt.Errorf("%w: launch reference does not belong to process group %d", ErrUnverifiableLaunch, pgid)
		}
		present, err := signalDarwinProcessWith(ctx, ref, signal, observe, send)
		if err != nil {
			return err
		}
		signaled = signaled || present
	}
	if !signaled {
		return errProcessNoSuchEntity
	}
	return nil
}

func processGroupExists(pgid int) (bool, error) {
	err := unix.Kill(-pgid, 0)
	if err == nil || errors.Is(err, unix.EPERM) {
		return true, nil
	}
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	return false, err
}

func processGroupMembersPresent(pgid int, refs []LaunchRef) (bool, error) {
	return processGroupMembersPresentWith(pgid, refs, observeProcessIdentity, processGroupID)
}

func discoverProcessScopeLaunches(ctx context.Context, identifier string, roots []LaunchRef, retained []LaunchRef, owner *ProcessRef, anchors []ProcessRef, pendingStart bool) ([]LaunchRef, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if len(roots) == 0 && len(retained) == 0 && !pendingStart {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("inspect Darwin process table: %w", err)
	}
	snapshots, err := snapshotDarwinProcessScope(ctx, identifier, processes, pendingStart)
	if err != nil {
		return nil, err
	}
	return discoverDarwinOwnedProcessLaunches(roots, retained, owner, anchors, snapshots)
}

func snapshotDarwinProcessScope(
	ctx context.Context,
	identifier string,
	processes []unix.KinfoProc,
	pendingStart bool,
) ([]processScopeSnapshot, error) {
	snapshots := make([]processScopeSnapshot, 0, len(processes))
	effectiveUID := uint32(unix.Geteuid())
	for _, process := range processes {
		snapshot, include, err := snapshotDarwinProcess(ctx, identifier, process, effectiveUID, pendingStart)
		if err != nil {
			return nil, err
		}
		if !include {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func snapshotDarwinProcess(
	ctx context.Context,
	identifier string,
	process unix.KinfoProc,
	effectiveUID uint32,
	pendingStart bool,
) (processScopeSnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return processScopeSnapshot{}, false, err
	}
	pid := int(process.Proc.P_pid)
	if pid <= 1 || process.Proc.P_stat == 5 || process.Eproc.Ucred.Uid != effectiveUID {
		return processScopeSnapshot{}, false, nil
	}
	sessionID, err := unix.Getsid(pid)
	if errors.Is(err, unix.ESRCH) {
		return processScopeSnapshot{}, false, nil
	}
	if err != nil {
		return processScopeSnapshot{}, false, fmt.Errorf("inspect Darwin session for PID %d: %w", pid, err)
	}
	present, identity, err := inspectDarwinCensusIdentity(pid, process)
	if err != nil {
		return processScopeSnapshot{}, false, fmt.Errorf("inspect Darwin identity for PID %d: %w", pid, err)
	}
	if !present {
		return processScopeSnapshot{}, false, nil
	}
	marked, err := darwinProcessHasScopeMarker(ctx, pid, identity.startToken, identifier, pendingStart)
	if err != nil {
		return processScopeSnapshot{}, false, err
	}
	return processScopeSnapshot{
		pid:         pid,
		parentPID:   int(process.Eproc.Ppid),
		pgid:        int(process.Eproc.Pgid),
		sessionID:   sessionID,
		startToken:  identity.startToken,
		scopeMarked: marked,
		zombie:      identity.zombie,
	}, true, nil
}

func inspectDarwinCensusIdentity(pid int, process unix.KinfoProc) (bool, darwinProcessIdentity, error) {
	identity, err := darwinProcessIdentityFromKinfo(pid, process, nativeDarwinProcInfo)
	if err == nil {
		return true, identity, nil
	}
	if errors.Is(err, unix.ESRCH) {
		return false, darwinProcessIdentity{}, nil
	}
	if !errors.Is(err, unix.EIO) {
		return false, darwinProcessIdentity{}, err
	}

	// The all-process table can outlive one entry long enough for its exact
	// proc_info read to race with exit. Reobserve the exact PID and accept the
	// refreshed identity only if it still matches the table entry's checked
	// start timestamp. A reused PID is deferred to the next complete census.
	present, refreshed, refreshErr := inspectDarwinProcessIdentityInfo(pid)
	if refreshErr != nil || !present {
		return false, darwinProcessIdentity{}, refreshErr
	}
	if !processIdentityTokensMatch(refreshed.startToken, darwinProcessStartToken(process.Proc.P_starttime)) {
		return false, darwinProcessIdentity{}, nil
	}
	return true, refreshed, nil
}

func discoverDarwinOwnedProcessLaunches(
	roots []LaunchRef,
	retained []LaunchRef,
	_ *ProcessRef,
	anchors []ProcessRef,
	snapshots []processScopeSnapshot,
) ([]LaunchRef, error) {
	// Darwin has no subreaper boundary. The caller is only an observer; using
	// it as an ownership-tree root would claim unrelated children created by
	// the same application. Durable roots, tracked anchors, markers, and their
	// observed topology provide the scope boundary instead.
	return discoverOwnedProcessLaunchesWithOwner(roots, retained, nil, anchors, snapshots)
}

func darwinProcessStartToken(start unix.Timeval) string {
	return "darwin:" + strconv.FormatInt(start.Sec, 10) + ":" + strconv.FormatInt(int64(start.Usec), 10)
}

func darwinProcessLaunchToken(start unix.Timeval, uniqueID uint64) string {
	return darwinProcessIdentityTokenVersion + ":" +
		strconv.FormatInt(start.Sec, 10) + ":" +
		strconv.FormatInt(int64(start.Usec), 10) + ":" +
		strconv.FormatUint(uniqueID, 10)
}
