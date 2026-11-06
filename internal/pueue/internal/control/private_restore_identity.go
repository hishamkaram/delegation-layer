package control

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func checkPrivateRestoreDirectory(ctx context.Context, parent, held *os.File, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var descriptor, named unix.Stat_t
	if err := unix.Fstat(int(held.Fd()), &descriptor); err != nil {
		return err
	}
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&descriptor, os.Geteuid(), true) != taskdir.PrivateStatAccepted || taskdir.CheckPrivateStat(&named, os.Geteuid(), true) != taskdir.PrivateStatAccepted || descriptor.Dev != named.Dev || descriptor.Ino != named.Ino {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}

func privateRestoreFileSource(file *os.File, config bool) (PrivateRestoreSource, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return PrivateRestoreSource{}, err
	}
	mode := uint32(stat.Mode)
	if int64(stat.Uid) != int64(os.Geteuid()) || mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || mode&0o7111 != 0 {
		return PrivateRestoreSource{}, task.ErrEvidenceFault
	}
	if config && taskdir.CheckPrivateStat(&stat, os.Geteuid(), false) != taskdir.PrivateStatAccepted {
		return PrivateRestoreSource{}, task.ErrEvidenceFault
	}
	info, err := file.Stat()
	if err != nil {
		return PrivateRestoreSource{}, err
	}
	return PrivateRestoreSource{device: uint64(stat.Dev), inode: uint64(stat.Ino), links: uint64(stat.Nlink), size: info.Size(), modified: info.ModTime().UnixNano(), mode: mode}, nil
}

func checkPrivateRestoreFile(ctx context.Context, parent, held *os.File, name string, expected PrivateRestoreSource, config bool) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := privateRestoreFileSource(held, config)
	if err != nil {
		return err
	}
	if current != expected {
		return task.ErrIdentityMismatch
	}
	named, err := openRepairChild(parent, name, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, named.Close()) }()
	visible, err := privateRestoreFileSource(named, config)
	if err != nil {
		return err
	}
	if visible != expected {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}
