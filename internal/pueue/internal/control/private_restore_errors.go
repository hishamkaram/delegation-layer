package control

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Raw input owns operational read facts independently of a parser, which may
// report a syntax verdict before consuming an error delivered beside bytes.
type privateRestoreInputReader struct {
	reader io.Reader
	fault  error
}

type privateRestoreReadError struct{ cause error }

func (e *privateRestoreReadError) Error() string { return e.cause.Error() }
func (e *privateRestoreReadError) Unwrap() error { return e.cause }

func (r *privateRestoreInputReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if err != nil && !task.AllErrorCauses(err, privateRestoreEOF) {
		err = &privateRestoreReadError{cause: err}
		r.fault = errors.Join(r.fault, err)
	}
	return n, err
}

func privateRestoreFault(kind, cause error) error {
	if cause == nil || privateRestoreOperationalFault(cause) {
		return cause
	}
	return errors.Join(kind, cause)
}

func privateRestoreOperationalFault(cause error) bool {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return true
	}
	var input *privateRestoreReadError
	if errors.As(cause, &input) && input != nil {
		return true
	}
	var path *os.PathError
	return errors.As(cause, &path) && path != nil && (path.Op == "read" || path.Op == "readat")
}
