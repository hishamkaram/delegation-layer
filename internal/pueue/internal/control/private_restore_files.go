package control

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type privateRestoreFiles struct {
	pin                             taskdir.RootPin
	root, base, config, data, state *os.File
	configSource, source            PrivateRestoreSource
	configDigest, name              string
	compressed                      bool
}

func (f privateRestoreFiles) close() error {
	return closeControlLeaseFiles([]*os.File{f.state, f.data, f.config, f.base, f.root})
}

func openPrivateRestore(ctx context.Context, pin taskdir.RootPin, expected task.SupervisorRef, resolution ResolutionContext) (f privateRestoreFiles, err error) {
	f.pin = pin
	f.root, err = openRepairPath(pin.Root())
	if err != nil {
		return f, err
	}
	if err = f.checkRoot(ctx); err != nil {
		return f, err
	}
	f.base, err = openRepairChild(f.root, privateSupervisorDirectory, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return f, err
	}
	if err = checkPrivateRestoreDirectory(ctx, f.root, f.base, privateSupervisorDirectory); err != nil {
		return f, err
	}
	f.config, err = openRepairChild(f.base, "pueue.yml", unix.O_RDONLY)
	if err != nil {
		return f, err
	}
	if f.configSource, err = privateRestoreFileSource(f.config, true); err != nil {
		return f, err
	}
	if err = f.readConfig(ctx, expected, resolution); err != nil {
		return f, err
	}
	f.name = "state.json"
	if f.compressed {
		f.name += ".gz"
	}
	f.data, err = openRepairChild(f.base, "data", unix.O_RDONLY|unix.O_DIRECTORY)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err = checkPrivateRestoreDirectory(ctx, f.base, f.data, "data"); err != nil {
		return f, err
	}
	if err = checkPrivateRestorePartial(ctx, f.data, f.name); err != nil {
		return f, err
	}
	f.state, err = openRepairChild(f.data, f.name, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err == nil {
		f.source, err = privateRestoreFileSource(f.state, false)
	}
	return f, err
}

func (f *privateRestoreFiles) readConfig(ctx context.Context, expected task.SupervisorRef, resolution ResolutionContext) error {
	bytes, err := task.ReadBounded(privateRestoreReader{ctx, &privateRestoreInputReader{reader: f.config}}, MaxControlBytes)
	if err != nil {
		return err
	}
	f.configDigest = task.ComputeSHA256(bytes)
	if f.configDigest != expected.ConfigDigest || expected.ResolvedConfigSHA256 == "" {
		return ErrBinding
	}
	resolved, err := resolvePrivateConfigSource(bytes, filepath.Join(f.pin.Root(), privateSupervisorDirectory), resolution)
	if err != nil {
		return err
	}
	digest, err := resolved.Digest()
	if err != nil {
		return err
	}
	if digest != expected.ResolvedConfigSHA256 || resolved.Endpoint() != expected.Endpoint || resolved.PueueDirectory != filepath.Join(f.pin.Root(), privateSupervisorDirectory, "data") {
		return ErrBinding
	}
	f.compressed = resolved.Daemon.CompressStateFile
	return nil
}

func (f privateRestoreFiles) check(ctx context.Context) error {
	if err := f.checkRoot(ctx); err != nil {
		return err
	}
	if err := checkPrivateRestoreDirectory(ctx, f.root, f.base, privateSupervisorDirectory); err != nil {
		return err
	}
	if err := checkPrivateRestoreFile(ctx, f.base, f.config, "pueue.yml", f.configSource, true); err != nil {
		return err
	}
	bytes, err := task.ReadBounded(privateRestoreReader{ctx, &privateRestoreInputReader{reader: io.NewSectionReader(f.config, 0, MaxControlBytes+1)}}, MaxControlBytes)
	if err != nil {
		return err
	}
	if task.ComputeSHA256(bytes) != f.configDigest {
		return task.ErrIdentityMismatch
	}
	if f.data == nil {
		return privateRestoreAbsent(ctx, f.base, "data")
	}
	if err = checkPrivateRestoreDirectory(ctx, f.base, f.data, "data"); err != nil {
		return err
	}
	if err = checkPrivateRestorePartial(ctx, f.data, f.name); err != nil {
		return err
	}
	if f.state == nil {
		return privateRestoreAbsent(ctx, f.data, f.name)
	}
	return checkPrivateRestoreFile(ctx, f.data, f.state, f.name, f.source, false)
}

func (f privateRestoreFiles) checkRoot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var held, named unix.Stat_t
	if err := unix.Fstat(int(f.root.Fd()), &held); err != nil {
		return err
	}
	if err := unix.Lstat(f.pin.Root(), &named); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&held, os.Geteuid(), true) != taskdir.PrivateStatAccepted || taskdir.CheckPrivateStat(&named, os.Geteuid(), true) != taskdir.PrivateStatAccepted || uint64(held.Dev) != f.pin.Device() || uint64(held.Ino) != f.pin.Inode() || held.Dev != named.Dev || held.Ino != named.Ino {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}

func checkPrivateRestorePartial(ctx context.Context, parent *os.File, name string) error {
	err := privateRestoreAbsent(ctx, parent, name+".partial")
	if err != nil {
		return privateRestoreFault(ErrPrivateRestorePartial, err)
	}
	return nil
}

func privateRestoreAbsent(ctx context.Context, parent *os.File, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var named unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, os.ErrNotExist) {
		return ctx.Err()
	}
	if err == nil {
		return task.ErrIdentityMismatch
	}
	return err
}
