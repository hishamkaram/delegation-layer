package control

import (
	"errors"
	"io"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// commandCompletion is the single whole-error classification of a supervisor
// command error. The control owner's wire encoding and private endpoint
// readiness both consume it, so an independent fault can never be reported as
// an ordinary completion on one path and as a fault on the other. It reads
// only error identities, never error text. errno is set only for
// commandCompletionErrno.
type commandCompletion struct {
	kind  commandCompletionKind
	errno syscall.Errno
}

type commandCompletionKind uint8

const (
	// commandCompletionOperational is any tree with an independent fault: a
	// reader, protocol, capture, signal, or mixed exit and errno leaf.
	commandCompletionOperational commandCompletionKind = iota
	// commandCompletionExit is exactly one natural nonzero exit, optionally
	// with the bounded empty-status pair ErrUnknown and io.EOF that the status
	// parser reports for a stream with no JSON value.
	commandCompletionExit
	// commandCompletionErrno is exactly one errno, including through its
	// standard path, syscall, or network wrappers, and nothing else.
	commandCompletionErrno
)

// commandCompletionLeaves counts the leaves of one error tree. ErrControlLimit
// is counted but does not change the classification; callers carry or reject
// it explicitly.
type commandCompletionLeaves struct {
	exits   int
	errnos  int
	unknown int
	eof     int
	errno   syscall.Errno
}

func (leaves *commandCompletionLeaves) accept(cause error) bool {
	var exited interface {
		ExitCode() int
		Exited() bool
	}
	var errno syscall.Errno
	switch {
	case errors.As(cause, &exited):
		leaves.exits++
		return exited.Exited() && exited.ExitCode() > 0
	case errors.As(cause, &errno):
		leaves.errnos++
		leaves.errno = errno
		return true
	case errors.Is(cause, ErrUnknown):
		leaves.unknown++
		return true
	case errors.Is(cause, io.EOF):
		leaves.eof++
		return true
	default:
		return errors.Is(cause, ErrControlLimit)
	}
}

// classifyCommandCompletion classifies err as a whole.
func classifyCommandCompletion(err error) commandCompletion {
	var leaves commandCompletionLeaves
	if !task.AllErrorCauses(err, leaves.accept) {
		return commandCompletion{kind: commandCompletionOperational}
	}
	switch {
	case leaves.exits == 1 && leaves.errnos == 0 && leaves.unknown == leaves.eof && leaves.unknown <= 1:
		return commandCompletion{kind: commandCompletionExit}
	case leaves.errnos == 1 && leaves.exits == 0 && leaves.unknown == 0 && leaves.eof == 0:
		return commandCompletion{kind: commandCompletionErrno, errno: leaves.errno}
	default:
		return commandCompletion{kind: commandCompletionOperational}
	}
}

// commandCompletionRecordKind is the owner's wire kind for a failed result.
// Only a whole-error ordinary completion or pure errno keeps its typed kind.
// Any independent reader, protocol, or mixed fault stays operational so the
// reader can never see it as a clean exit or errno.
func commandCompletionRecordKind(result CommandResult) (kind string, errno int) {
	completion := classifyCommandCompletion(result.Err)
	switch {
	case completion.kind == commandCompletionExit && result.Started:
		return "exit", 0
	case completion.kind == commandCompletionErrno:
		return "errno", int(completion.errno)
	case !result.Started:
		return "start", 0
	default:
		return "wait", 0
	}
}

// validateControlOwnerErrorFields requires an error kind exactly when an error
// is present, an error for every nonzero exit, and a natural nonzero exit for
// the exit kind.
func validateControlOwnerErrorFields(record *controlOwnerResultRecord) error {
	if record.ErrorMessage == "" {
		if record.ErrorKind != "" || record.Errno != 0 || (record.Started && record.ExitCode > 0) {
			return ErrConfiguration
		}
		return nil
	}
	if record.ErrorKind == "" || (record.ErrorKind == "exit" && (!record.Started || record.ExitCode <= 0)) {
		return ErrConfiguration
	}
	return nil
}

// controlErrnoError is a pure errno received from the owner. It keeps the
// owner's diagnostic text and unwraps only to the errno, so the reader
// classifies it exactly as the owner did.
type controlErrnoError struct {
	errno   syscall.Errno
	message string
}

func (e *controlErrnoError) Error() string { return e.message }

func (e *controlErrnoError) Unwrap() error { return e.errno }

func controlOwnerErrorFromRecord(record *controlOwnerResultRecord) (error, error) {
	if record.ErrorMessage == "" {
		if record.ControlLimit {
			return ErrControlLimit, nil
		}
		return nil, nil
	}
	var resultErr error
	switch record.ErrorKind {
	case "exit":
		resultErr = &controlExitError{code: record.ExitCode, message: record.ErrorMessage}
	case "errno":
		if record.Errno <= 0 {
			return nil, ErrConfiguration
		}
		resultErr = &controlErrnoError{errno: syscall.Errno(record.Errno), message: record.ErrorMessage}
	case "start", "wait":
		resultErr = errors.New(record.ErrorMessage)
	default:
		return nil, ErrConfiguration
	}
	if record.ControlLimit {
		resultErr = errors.Join(resultErr, ErrControlLimit)
	}
	return resultErr, nil
}
