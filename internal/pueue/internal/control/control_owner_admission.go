package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

var (
	ErrRootAdmissionRequired       = errors.New("root admission shared lease is required")
	ErrManagedAdmissionUnsupported = errors.New("managed admission requires staged submission")
)

type controlAdmissionRole string

const (
	controlAdmissionAdd     controlAdmissionRole = "staged-add"
	controlAdmissionEnqueue controlAdmissionRole = "staged-enqueue"
	controlAdmissionRepair  controlAdmissionRole = "managed-repair"
)

type controlAdmissionKey struct{}

// The resource remains the sole SH owner. This immutable, command-local view
// selects its actual duplicate; it grants neither submission nor native Start.
type controlAdmissionScope struct {
	lease        *taskdir.RootAdmissionLease
	pin          taskdir.RootPin
	role         controlAdmissionRole
	configPath   string
	repairParent *controlRepairParent
}

type controlOwnerAdmission struct {
	Role         controlAdmissionRole `json:"role"`
	Root         string               `json:"root"`
	RootID       string               `json:"root_id"`
	Directory    controlLeaseIdentity `json:"directory"`
	Lock         controlLeaseIdentity `json:"lock"`
	ConfigPath   string               `json:"config_path"`
	LeaseIndex   int                  `json:"lease_index"`
	RepairParent *controlRepairParent `json:"repair_parent,omitempty"`
}

type controlLeaseDuplicates struct {
	files     []*os.File
	admission *controlOwnerAdmission
}

func rootAdmissionCandidate(ctx context.Context) (*taskdir.RootAdmissionLease, int) {
	var selected *taskdir.RootAdmissionLease
	count := 0
	for _, lease := range controlLeaseContextFrom(ctx).extra {
		if actual, ok := lease.(*taskdir.RootAdmissionLease); ok {
			selected = actual
			count++
		}
	}
	return selected, count
}

func sameRootAdmissionLease(left, right ControlLease) bool {
	actualLeft, leftOK := left.(*taskdir.RootAdmissionLease)
	actualRight, rightOK := right.(*taskdir.RootAdmissionLease)
	return leftOK && rightOK && actualLeft == actualRight
}

func admissionScopeFrom(ctx context.Context) *controlAdmissionScope {
	if ctx == nil {
		return nil
	}
	scope, ok := ctx.Value(controlAdmissionKey{}).(*controlAdmissionScope)
	if !ok {
		return nil
	}
	return scope
}

func isScopedRootAdmissionLease(lease ControlLease, scope *controlAdmissionScope) bool {
	if scope == nil {
		return false
	}
	actual, ok := lease.(*taskdir.RootAdmissionLease)
	return ok && actual == scope.lease
}

func (c *Client) requireRootAdmissionOwner(ctx context.Context) error {
	_, count := rootAdmissionCandidate(ctx)
	if count != 0 && c.options.ControlOwnerExecutable == "" {
		return fmt.Errorf("%w: external control owner is required", ErrRootAdmissionRequired)
	}
	return nil
}

func controlOwnerShutdownSelection(ctx context.Context, duplicates controlLeaseDuplicates) (*controlOwnerShutdown, error) {
	shutdown, err := privateShutdownForwarding(ctx, duplicates.files)
	if err != nil {
		return nil, err
	}
	if shutdown != nil && duplicates.admission != nil {
		return nil, ErrConfiguration
	}
	return shutdown, nil
}

func (c *Client) admissionResource(ctx context.Context, rootID, launchRoot string) (*taskdir.RootAdmissionLease, taskdir.RootPin, error) {
	if ctx == nil {
		return nil, taskdir.RootPin{}, context.Canceled
	}
	lease, count := rootAdmissionCandidate(ctx)
	if count == 0 && c.options.ControlOwnerExecutable == "" {
		return nil, taskdir.RootPin{}, nil
	}
	if count != 1 {
		return nil, taskdir.RootPin{}, fmt.Errorf("%w: expected exactly one actual resource, found %d", ErrRootAdmissionRequired, count)
	}
	if c.options.ControlOwnerExecutable == "" {
		return nil, taskdir.RootPin{}, fmt.Errorf("%w: external control owner is required", ErrRootAdmissionRequired)
	}
	pin, err := lease.PinRootContext(ctx)
	if err == nil {
		err = matchAdmissionRoot(ctx, pin, rootID, launchRoot)
	}
	if err != nil {
		return nil, taskdir.RootPin{}, errors.Join(ErrRootAdmissionRequired, err)
	}
	return lease, pin, nil
}

func (c *Client) checkAdmissionResource(ctx context.Context, rootID, launchRoot string) error {
	_, _, err := c.admissionResource(ctx, rootID, launchRoot)
	return err
}

func (c *Client) admissionCommandContext(ctx context.Context, role controlAdmissionRole, rootID, launchRoot string) (context.Context, error) {
	lease, pin, err := c.admissionResource(ctx, rootID, launchRoot)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return ctx, nil
	}
	return context.WithValue(ctx, controlAdmissionKey{}, &controlAdmissionScope{lease: lease, pin: pin, role: role, configPath: c.runtimeBinding.ConfigPath}), nil
}

func matchAdmissionRoot(ctx context.Context, pin taskdir.RootPin, rootID, launchRoot string) error {
	if err := task.ValidateRootID(rootID); err != nil {
		return err
	}
	if pin.RootID() != rootID {
		return task.ErrIdentityMismatch
	}
	if launchRoot == "" {
		return nil
	}
	other, err := readControlRootPin(ctx, launchRoot)
	if err != nil {
		return err
	}
	// Path validity is checked separately. Native identity admits supported
	// spelling aliases without treating a same-ID copied directory as equal.
	if other.RootID() != pin.RootID() || other.Device() != pin.Device() || other.Inode() != pin.Inode() {
		return task.ErrIdentityMismatch
	}
	return nil
}

func readControlRootPin(ctx context.Context, root string) (pin taskdir.RootPin, resultErr error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return pin, ErrConfiguration
	}
	if err := rejectSymlinkComponents(root); err != nil {
		return pin, err
	}
	// Metadata-only canonical reads: no probe, maintenance lock or task policy
	// is used. The supplied immutable registry is unused by these root reads.
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, predicate.Default())
	if err != nil {
		return pin, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	return reader.PinRootContext(ctx)
}

func admissionFileIdentity(root string, file *os.File) (controlLeaseIdentity, error) {
	var descriptor, named unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &descriptor); err != nil {
		return controlLeaseIdentity{}, err
	}
	if err := unix.Lstat(filepath.Join(root, taskdir.RootAdmissionLockName), &named); err != nil {
		return controlLeaseIdentity{}, err
	}
	if taskdir.CheckPrivateStat(&descriptor, os.Geteuid(), false) != taskdir.PrivateStatAccepted || taskdir.CheckPrivateStat(&named, os.Geteuid(), false) != taskdir.PrivateStatAccepted {
		return controlLeaseIdentity{}, task.ErrEvidenceFault
	}
	identity := controlLeaseIdentity{Device: uint64(descriptor.Dev), Inode: descriptor.Ino}
	if identity != (controlLeaseIdentity{Device: uint64(named.Dev), Inode: named.Ino}) {
		return controlLeaseIdentity{}, task.ErrIdentityMismatch
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return controlLeaseIdentity{}, err
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return controlLeaseIdentity{}, task.ErrEvidenceFault
	}
	return identity, nil
}

func admissionDuplicate(ctx context.Context, scope *controlAdmissionScope, file *os.File, index int) (*controlOwnerAdmission, error) {
	pin, err := scope.lease.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	if !pin.Equal(scope.pin) {
		return nil, task.ErrIdentityMismatch
	}
	identity, err := admissionFileIdentity(pin.Root(), file)
	if err != nil {
		return nil, err
	}
	after, err := scope.lease.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	if !pin.Equal(after) {
		return nil, task.ErrIdentityMismatch
	}
	return &controlOwnerAdmission{Role: scope.role, Root: pin.Root(), RootID: pin.RootID(), Directory: controlLeaseIdentity{Device: pin.Device(), Inode: pin.Inode()}, Lock: identity, ConfigPath: scope.configPath, LeaseIndex: index, RepairParent: scope.repairParent}, nil
}

func validateControlOwnerForwarding(request controlOwnerRequest) error {
	if request.Shutdown != nil && request.Admission != nil {
		return ErrConfiguration
	}
	if err := validateControlOwnerShutdown(request); err != nil {
		return err
	}
	if request.Admission == nil {
		return nil
	}
	if err := validateControlAdmissionHeader(request); err != nil {
		return err
	}
	admission := request.Admission
	switch admission.Role {
	case controlAdmissionAdd:
		return validateAdmissionAddArguments(request.Arguments[3:], admission.RootID)
	case controlAdmissionEnqueue:
		return validateAdmissionNumericCommand(request.Arguments, "enqueue")
	case controlAdmissionRepair:
		if err := validateRepairParentHeader(admission); err != nil {
			return err
		}
		return validateAdmissionNumericCommand(request.Arguments, "edit")
	default:
		return ErrConfiguration
	}
}

func validateAdmissionNumericCommand(arguments []string, verb string) error {
	if len(arguments) != 5 || arguments[3] != verb {
		return ErrConfiguration
	}
	id, err := strconv.ParseInt(arguments[4], 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10) != arguments[4] {
		return ErrConfiguration
	}
	return nil
}

func validateControlAdmissionHeader(request controlOwnerRequest) error {
	admission := request.Admission
	if admission.Role != controlAdmissionRepair && admission.RepairParent != nil {
		return ErrConfiguration
	}
	if !filepath.IsAbs(admission.Root) || filepath.Clean(admission.Root) != admission.Root || task.ValidateRootID(admission.RootID) != nil || admission.Directory.Inode == 0 || admission.Lock.Inode == 0 || admission.LeaseIndex < 0 || request.StatusVersion != "" {
		return ErrConfiguration
	}
	if len(request.Arguments) < 4 || request.Arguments[1] != "-c" || request.Arguments[2] != admission.ConfigPath || !filepath.IsAbs(admission.ConfigPath) || filepath.Clean(admission.ConfigPath) != admission.ConfigPath {
		return ErrConfiguration
	}
	return nil
}

type admissionAddParts struct {
	label  string
	group  string
	worker []string
}

func splitAdmissionAddArguments(arguments []string) (admissionAddParts, error) {
	if len(arguments) < 11 || arguments[0] != "add" || arguments[1] != "--escape" || arguments[2] != "--stashed" {
		return admissionAddParts{}, ErrConfiguration
	}
	position, group := 3, ""
	if arguments[position] == "--group" {
		if len(arguments) < 13 {
			return admissionAddParts{}, ErrConfiguration
		}
		group, position = arguments[position+1], position+2
		if group == "" {
			return admissionAddParts{}, ErrConfiguration
		}
	}
	if arguments[position] != "--label" || arguments[position+2] != "--print-task-id" || arguments[position+3] != "--" {
		return admissionAddParts{}, ErrConfiguration
	}
	return admissionAddParts{label: arguments[position+1], group: group, worker: arguments[position+4:]}, nil
}

func validateAdmissionWorker(worker []string, auxiliary bool) (string, error) {
	if len(worker) < 4 {
		return "", ErrConfiguration
	}
	if !filepath.IsAbs(worker[0]) || filepath.Clean(worker[0]) != worker[0] || worker[len(worker)-3] != "--root" || !filepath.IsAbs(worker[len(worker)-2]) || filepath.Clean(worker[len(worker)-2]) != worker[len(worker)-2] {
		return "", ErrConfiguration
	}
	taskID := worker[len(worker)-1]
	if err := task.ValidateTaskID(taskID); err != nil {
		return "", err
	}
	if err := validateAdmissionWorkerPrefix(worker[1:len(worker)-3], auxiliary); err != nil {
		return "", err
	}
	return taskID, nil
}

func validateAdmissionWorkerPrefix(prefix []string, auxiliary bool) error {
	if auxiliary {
		if len(prefix) == 0 || (prefix[0] != "--inspection" && prefix[0] != "--workspace-operation") {
			return ErrConfiguration
		}
		prefix = prefix[1:]
	}
	if len(prefix) != 0 && (len(prefix) != 2 || prefix[0] != "--guardian" || prefix[1] != "--supervised") {
		return ErrConfiguration
	}
	return nil
}

func validateAdmissionAddArguments(arguments []string, rootID string) error {
	parts, err := splitAdmissionAddArguments(arguments)
	if err != nil {
		return err
	}
	taskID, err := validateAdmissionWorker(parts.worker, parts.group != "")
	if err != nil {
		return err
	}
	label := (Identity{RootID: rootID, TaskID: taskID}).Label()
	if parts.group != "" {
		identity := InspectionIdentity{RootID: rootID, TaskID: taskID, QueueGroup: parts.group}
		if err = validateInspectionIdentity(identity); err != nil {
			return err
		}
		label = identity.Label()
	}
	if parts.label != label {
		return task.ErrIdentityMismatch
	}
	return nil
}

func controlOwnerForwardedFiles(request controlOwnerRequest, files []*os.File) ([]*os.File, error) {
	if request.Admission == nil {
		return controlOwnerShutdownFiles(request, files)
	}
	if request.Shutdown != nil {
		return nil, ErrConfiguration
	}
	admission := request.Admission
	if admission.LeaseIndex < 0 || admission.LeaseIndex >= len(files) {
		return nil, ErrConfiguration
	}
	// An admitted managed command owns its completion independently of the
	// caller's finite observation context. These are read-only native checks.
	ctx := context.Background()
	pin, err := readControlRootPin(ctx, admission.Root)
	if err != nil {
		return nil, err
	}
	if pin.RootID() != admission.RootID || pin.Device() != admission.Directory.Device || pin.Inode() != admission.Directory.Inode {
		return nil, task.ErrIdentityMismatch
	}
	file := files[admission.LeaseIndex]
	identity, err := admissionFileIdentity(pin.Root(), file)
	if err != nil {
		return nil, err
	}
	if identity != admission.Lock {
		return nil, task.ErrIdentityMismatch
	}
	if err = validateForwardedAdmissionSubject(ctx, pin, admission, request.Arguments); err != nil {
		return nil, err
	}
	after, err := readControlRootPin(ctx, admission.Root)
	if err != nil {
		return nil, err
	}
	if !pin.Equal(after) {
		return nil, task.ErrIdentityMismatch
	}
	return []*os.File{file}, nil
}

func validateForwardedAdmissionSubject(ctx context.Context, pin taskdir.RootPin, admission *controlOwnerAdmission, arguments []string) error {
	switch admission.Role {
	case controlAdmissionAdd:
		return matchAdmissionRoot(ctx, pin, admission.RootID, arguments[len(arguments)-2])
	case controlAdmissionRepair:
		return verifyRepairParent(admission.RepairParent)
	case controlAdmissionEnqueue:
		return nil
	default:
		return ErrConfiguration
	}
}
