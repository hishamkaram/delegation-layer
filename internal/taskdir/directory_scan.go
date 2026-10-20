package taskdir

import (
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// directoryNames is the part of an opened directory a streaming scan uses.
// *os.File satisfies it; callers keep opening and missing-directory handling.
type directoryNames interface {
	Readdirnames(n int) ([]string, error)
	Close() error
}

// scanDirectoryNames streams a directory one name at a time to visit and
// closes it exactly once. Names returned with a read error are visited before
// that error is considered. A visitor, read, close or cancellation fault stops
// the scan and every cause is joined; only a read error whose every cause is
// io.EOF ends the scan cleanly. canceled reports the caller's cancellation
// before each read and after Close; a caller without a context passes
// notCanceled.
func scanDirectoryNames(canceled func() error, dir directoryNames, visit func(string) error) error {
	var err error
	for {
		if err = canceled(); err != nil {
			return errors.Join(err, dir.Close())
		}
		names, readErr := dir.Readdirnames(1)
		for _, name := range names {
			if err = visit(name); err != nil {
				break
			}
		}
		end := readErr != nil && task.AllErrorCauses(readErr, isEndOfDirectory)
		if readErr != nil && !end {
			err = errors.Join(err, readErr)
		}
		if err != nil || end {
			break
		}
	}
	return errors.Join(err, dir.Close(), canceled())
}

func notCanceled() error {
	return nil
}

func isEndOfDirectory(cause error) bool {
	return errors.Is(cause, io.EOF)
}
