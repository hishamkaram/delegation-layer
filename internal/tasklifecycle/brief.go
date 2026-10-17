package tasklifecycle

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ReadBrief(path string) (data []byte, resultErr error) {
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

func readBriefFileForRequest(path string, req task.TaskRecord) ([]byte, error) {
	briefPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	brief, err := ReadBrief(briefPath)
	if err != nil {
		return nil, err
	}
	if int64(len(brief)) != req.BriefLength || task.ComputeSHA256(brief) != req.BriefSHA256 {
		return nil, errors.New("brief changed after request preparation")
	}
	return brief, nil
}
