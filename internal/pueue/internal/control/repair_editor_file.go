package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func replaceRepairCommand(path, id, expected, replacement string, parent controlRepairParent) (resultErr error) {
	if expected == "" || replacement == "" || len(expected) > MaxControlBytes || len(replacement) > MaxControlBytes || strings.ContainsRune(replacement, 0) {
		return ErrConfiguration
	}
	files, err := openRepairCommandFiles(path, id, parent)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, files.close()) }()
	data, err := task.ReadBounded(files.command, MaxControlBytes)
	if err != nil {
		return err
	}
	if string(data) != expected {
		return task.ErrIdentityMismatch
	}
	if err = files.check(); err != nil {
		return err
	}
	if _, err = files.command.WriteAt([]byte(replacement), 0); err != nil {
		return err
	}
	if err = files.command.Truncate(int64(len(replacement))); err != nil {
		return err
	}
	return files.check()
}

type repairCommandFiles struct {
	path, id                 string
	parent                   controlRepairParent
	parentFile               *os.File
	root, directory, command *os.File
}

func (files repairCommandFiles) close() error {
	return closeControlLeaseFiles([]*os.File{files.command, files.directory, files.root, files.parentFile})
}

func (files repairCommandFiles) check() error {
	if err := checkRepairParent(files.parent, files.parentFile); err != nil {
		return err
	}
	if err := checkRepairEntry(files.parentFile, files.root, filepath.Base(files.path), true); err != nil {
		return err
	}
	return checkRepairChain(files.path, files.id, files.root, files.directory, files.command)
}

func openRepairCommandFiles(path, id string, parent controlRepairParent) (files repairCommandFiles, resultErr error) {
	files.path, files.id, files.parent = path, id, parent
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, files.close())
		}
	}()
	if filepath.Dir(path) != parent.Path {
		return files, ErrConfiguration
	}
	var err error
	files.parentFile, err = openRepairPath(parent.Path)
	if err != nil {
		return files, err
	}
	if err = checkRepairParent(parent, files.parentFile); err != nil {
		return files, err
	}
	files.root, err = openRepairChild(files.parentFile, filepath.Base(path), unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return files, err
	}
	if err = checkRepairEntry(files.parentFile, files.root, filepath.Base(path), true); err != nil {
		return files, err
	}
	if err = files.root.Chmod(0o700); err != nil {
		return files, err
	}
	files.directory, err = openRepairChild(files.root, id, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return files, err
	}
	if err = checkRepairEntry(files.root, files.directory, id, true); err != nil {
		return files, err
	}
	if err = files.directory.Chmod(0o700); err != nil {
		return files, err
	}
	files.command, err = openRepairChild(files.directory, "command", unix.O_RDWR)
	if err != nil {
		return files, err
	}
	if err = files.check(); err != nil {
		return files, err
	}
	return files, files.command.Chmod(0o600)
}

func checkRepairRoot(path string, root *os.File) error {
	var descriptor, named unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &descriptor); err != nil {
		return err
	}
	if err := unix.Lstat(path, &named); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&descriptor, os.Geteuid(), true) != taskdir.PrivateStatAccepted {
		return fmt.Errorf("%w: repair temporary directory must be private (mode %#o)", task.ErrEvidenceFault, uint32(descriptor.Mode)&0o7777)
	}
	if descriptor.Dev != named.Dev || descriptor.Ino != named.Ino || descriptor.Mode != named.Mode {
		return task.ErrIdentityMismatch
	}
	return nil
}

func checkRepairChain(path, id string, root, dir, file *os.File) error {
	if err := checkRepairRoot(path, root); err != nil {
		return err
	}
	if err := checkRepairEntry(root, dir, id, true); err != nil {
		return err
	}
	return checkRepairEntry(dir, file, "command", false)
}

func openRepairPath(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrConfiguration
	}
	current, err := os.Open(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		next, openErr := openRepairChild(current, part, unix.O_RDONLY|unix.O_DIRECTORY)
		closeErr := current.Close()
		if openErr != nil || closeErr != nil {
			if next != nil {
				closeErr = errors.Join(closeErr, next.Close())
			}
			return nil, errors.Join(openErr, closeErr)
		}
		current = next
	}
	return current, nil
}

func openRepairChild(parent *os.File, name string, flags int) (*os.File, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, ErrConfiguration
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func checkRepairEntry(parent, file *os.File, name string, directory bool) error {
	var descriptor, named unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &descriptor); err != nil {
		return err
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	kind := uint32(unix.S_IFREG)
	if directory {
		kind = unix.S_IFDIR
	}
	// Rust creates these entries with the caller's umask (0775/0664 with 0002).
	// The pinned private 0700 parent is the access boundary. Validate native
	// ownership and identity before narrowing the held descriptors for I/O.
	mode := uint32(descriptor.Mode)
	if int64(descriptor.Uid) != int64(os.Geteuid()) || mode&0o7000 != 0 || mode&unix.S_IFMT != kind || (!directory && descriptor.Nlink != 1) {
		return fmt.Errorf("%w: unsafe repair entry type, owner, special bits or link count", task.ErrEvidenceFault)
	}
	if descriptor.Dev != named.Dev || descriptor.Ino != named.Ino || descriptor.Mode != named.Mode {
		return task.ErrIdentityMismatch
	}
	return nil
}
