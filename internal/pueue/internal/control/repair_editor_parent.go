package control

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

const (
	repairParentEnv = "DELEGATE_PUEUE_EDIT_PARENT"
	repairDeviceEnv = "DELEGATE_PUEUE_EDIT_PARENT_DEVICE"
	repairInodeEnv  = "DELEGATE_PUEUE_EDIT_PARENT_INODE"
)

type controlRepairParent struct {
	Path     string               `json:"path"`
	Identity controlLeaseIdentity `json:"identity"`
}

// Retained native-edit policy: Pueue owns its temporary directory lifecycle.
// If its CLI is killed, abandoned .tmp* entries under this parent are retained
// and reported with the interrupted edit's diagnostics. A pathname or missing
// CLI alone never authorizes deletion: a surviving editor/descendant may still
// use it. Delegate adds no automatic scavenger without proven actor closure.

func canonicalRepairParent(root string) (controlRepairParent, error) {
	path := filepath.Join(root, privateSupervisorDirectory)
	file, err := openRepairPath(path)
	if err != nil {
		return controlRepairParent{}, err
	}
	var stat unix.Stat_t
	err = unix.Fstat(int(file.Fd()), &stat)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return controlRepairParent{}, errors.Join(err, closeErr)
	}
	if taskdir.CheckPrivateStat(&stat, os.Geteuid(), true) != taskdir.PrivateStatAccepted {
		return controlRepairParent{}, task.ErrEvidenceFault
	}
	return controlRepairParent{Path: path, Identity: controlLeaseIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}}, nil
}

func validateRepairParentHeader(admission *controlOwnerAdmission) error {
	parent := admission.RepairParent
	if parent == nil || parent.Path != filepath.Join(admission.Root, privateSupervisorDirectory) || parent.Path != filepath.Dir(admission.ConfigPath) || parent.Identity.Inode == 0 {
		return ErrConfiguration
	}
	return nil
}

func verifyRepairParent(parent *controlRepairParent) error {
	if parent == nil {
		return ErrConfiguration
	}
	file, err := openRepairPath(parent.Path)
	if err != nil {
		return err
	}
	return errors.Join(checkRepairParent(*parent, file), file.Close())
}

func checkRepairParent(parent controlRepairParent, file *os.File) error {
	var descriptor, named unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &descriptor); err != nil {
		return err
	}
	if err := unix.Lstat(parent.Path, &named); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&descriptor, os.Geteuid(), true) != taskdir.PrivateStatAccepted || taskdir.CheckPrivateStat(&named, os.Geteuid(), true) != taskdir.PrivateStatAccepted {
		return task.ErrEvidenceFault
	}
	if parent.Identity != (controlLeaseIdentity{Device: uint64(descriptor.Dev), Inode: descriptor.Ino}) || descriptor.Dev != named.Dev || descriptor.Ino != named.Ino {
		return task.ErrIdentityMismatch
	}
	return nil
}

func repairActuatorEnvironment(environment []string, parent *controlRepairParent) []string {
	values := setEnvironment(environment, "TMPDIR", parent.Path)
	values = setEnvironment(values, repairParentEnv, parent.Path)
	values = setEnvironment(values, repairDeviceEnv, strconv.FormatUint(parent.Identity.Device, 10))
	return setEnvironment(values, repairInodeEnv, strconv.FormatUint(parent.Identity.Inode, 10))
}

func controlOwnerEnvironment(admission *controlOwnerAdmission, forwardedCount int) ([]string, error) {
	environment := os.Environ()
	if admission == nil || admission.Role != controlAdmissionRepair {
		return environment, nil
	}
	// ExtraFiles[0] becomes fd3 in the Rust CLI, regardless of the original
	// selected lease index in the owner's larger inherited file set.
	if forwardedCount != 1 || admission.RepairParent == nil {
		return nil, ErrConfiguration
	}
	return repairActuatorEnvironment(environment, admission.RepairParent), nil
}

func repairParentFromEnvironment(root, path string) (controlRepairParent, error) {
	parent := controlRepairParent{Path: os.Getenv(repairParentEnv)}
	if parent.Path != filepath.Join(root, privateSupervisorDirectory) || filepath.Dir(path) != parent.Path {
		return parent, ErrConfiguration
	}
	device, deviceErr := strconv.ParseUint(os.Getenv(repairDeviceEnv), 10, 64)
	inode, inodeErr := strconv.ParseUint(os.Getenv(repairInodeEnv), 10, 64)
	if deviceErr != nil || inodeErr != nil || inode == 0 || strconv.FormatUint(device, 10) != os.Getenv(repairDeviceEnv) || strconv.FormatUint(inode, 10) != os.Getenv(repairInodeEnv) {
		return parent, ErrConfiguration
	}
	parent.Identity = controlLeaseIdentity{Device: device, Inode: inode}
	return parent, nil
}
