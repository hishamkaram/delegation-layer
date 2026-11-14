package gatewayservice

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func privateDirectory(info os.FileInfo, uid int, strict bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != uint32(uid) || info.Mode().Perm()&0o022 != 0 || strict && info.Mode().Perm() != 0o700 {
		return ErrConfiguration
	}
	return nil
}

func privateUnit(info os.FileInfo, uid int) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || stat.Uid != uint32(uid) || stat.Nlink != 1 {
		return ErrForeignService
	}
	return nil
}

func (m *Manager) unitPath() string { return filepath.Join(m.config.Directory, m.filename) }

func (m *Manager) checkServiceDirectory() error {
	canonical, err := config.CanonicalizePath(m.config.Directory)
	if err != nil || canonical != m.config.Directory {
		return errors.Join(ErrConfiguration, err)
	}
	info, err := os.Lstat(m.config.Directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return privateDirectory(info, m.config.UID, false)
}

func (m *Manager) readUnit() (present bool, resultErr error) {
	if err := m.checkServiceDirectory(); err != nil {
		return false, err
	}
	directory, err := os.OpenRoot(m.config.Directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	before, err := directory.Lstat(m.filename)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = privateUnit(before, m.config.UID); err != nil {
		return true, err
	}
	file, err := directory.OpenFile(m.filename, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return true, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return true, errors.Join(ErrForeignService, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxServiceDefinition+1))
	if err != nil {
		return true, err
	}
	return true, m.verifyDefinition(data)
}

func (m *Manager) installUnit() (resultErr error) {
	if !validPath(m.config.Path) || len(m.rendered) == 0 {
		return ErrConfiguration
	}
	if err := m.checkServiceDirectory(); err != nil {
		return err
	}
	if err := os.MkdirAll(m.config.Directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(m.config.Directory)
	if err != nil {
		return err
	}
	if err = privateDirectory(info, m.config.UID, false); err != nil {
		return err
	}
	directory, err := os.OpenRoot(m.config.Directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	held, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, held.Close()) }()
	stage := "." + m.filename + ".installing"
	if err = removeUnpublishedStage(directory, held, stage, m.config.UID); err != nil {
		return err
	}
	file, err := directory.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeUnpublishedStage(directory, held, stage, m.config.UID))
	}()
	_, writeErr := file.Write(m.rendered)
	resultErr = errors.Join(writeErr, taskdir.BarrierFile(file), file.Close())
	if resultErr != nil {
		return resultErr
	}
	// Atomic no-replace rename never creates a second link to the installed file.
	if err = renameNoReplace(int(held.Fd()), stage, m.filename); err != nil {
		return err
	}
	return m.barrierInstalledUnit()
}

func removeUnpublishedStage(directory *os.Root, held *os.File, name string, uid int) error {
	info, err := directory.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = privateUnit(info, uid); err != nil {
		return err
	}
	if err = directory.Remove(name); err != nil {
		return err
	}
	return taskdir.BarrierFile(held)
}

// removed records only a successful owned unlink, not durable absence. The
// caller must reconcile the manager cache even if the following barrier fails.
func (m *Manager) removeUnit() (removed bool, resultErr error) {
	present, err := m.readUnit()
	if err != nil {
		return false, err
	}
	if !present {
		return false, ErrForeignService
	}
	if err = os.Remove(m.unitPath()); err != nil {
		return false, err
	}
	return true, m.barrierUnitAbsence()
}
