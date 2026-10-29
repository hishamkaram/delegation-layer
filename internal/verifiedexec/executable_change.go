package verifiedexec

// Provenance of "the admitted executable changed". A caller that supplied the
// admitted path and digest may repair a binding refusal by re-admitting the
// executable only when the refusal says that the named path no longer holds the
// observed file or bytes. Nothing else may authorize that: a cache, interpreter,
// stage, view, gate, close, seek, read or copy fault is a refusal.
//
// Owner: verifiedexec. Producers: the places in Open and its helpers that
// observe the executable's own path, which tag a cause as an executableChangeError;
// and openAdmittedEntrypoint, the one open of the request's entrypoint used by
// NewCommandFromRequest and Pool.bindUnderGate, which converts a whole error to
// ErrAdmittedExecutableChanged. Reader: a caller of those two constructors.
// Invariant: ErrAdmittedExecutableChanged is reported only when every cause of
// opening the request's own entrypoint is an executableChangeError. Open, Fingerprint
// and LocatePath on their own never report it.

import (
	"errors"
	"io/fs"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ErrAdmittedExecutableChanged reports that a command binding was refused because
// the admitted entrypoint path no longer holds the file or bytes observed at
// admission: its bytes differ, it changed while being inspected, or it is gone.
// A caller classifies the whole returned error with task.AllErrorCauses; a
// joined close or I/O fault, a symlink, a non-executable file and every fault of
// an interpreter, cached image or stage never carry it.
var ErrAdmittedExecutableChanged = errors.New("admitted executable changed after admission")

// executableChangeError is one cause, observed on a named executable's own path, that
// the path no longer holds what was admitted. It is a leaf by design: it has no
// Unwrap, so a whole-error walk sees it as the leaf and its provenance reaches
// the classifier. Error keeps the original text; Is and As delegate to the
// original error so errors.Is(fs.ErrNotExist) and errors.As(*fs.PathError)
// answer as before. admitted is set only on the leaf made by admittedOpenError.
type executableChangeError struct {
	err      error
	admitted bool
}

func (c *executableChangeError) Error() string { return c.err.Error() }

func (c *executableChangeError) Is(target error) bool {
	return (c.admitted && target == ErrAdmittedExecutableChanged) || errors.Is(c.err, target)
}

// As forwards to errors.As. The signature is fixed by the errors package and its
// target is checked there before it reaches this method, so any is safe.
func (c *executableChangeError) As(target any) bool { return errors.As(c.err, target) }

// changedExecutable tags err as a change of the named executable.
func changedExecutable(err error) error { return &executableChangeError{err: err} }

// missingExecutable tags an error of an operation on the executable's own path
// when the path is gone. Any other error is returned untouched.
func missingExecutable(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return changedExecutable(err)
	}
	return err
}

// isExecutableChange is the leaf predicate of admittedOpenError: a tagged cause
// that is not itself already the admitted marker.
func isExecutableChange(cause error) bool {
	var change *executableChangeError
	return errors.As(cause, &change) && !change.admitted
}

// admittedOpenError is the pure classification of an entrypoint Open error: the
// whole error becomes one admitted leaf only if every leaf is an executableChangeError.
// Any other leaf, such as a joined close or I/O fault, leaves it unchanged.
func admittedOpenError(err error) error {
	if task.AllErrorCauses(err, isExecutableChange) {
		return &executableChangeError{err: err, admitted: true}
	}
	return err
}

// openAdmittedEntrypoint opens the request's own executable and verifies its
// digest. It is the only place that can mark a refusal as an admitted change.
func openAdmittedEntrypoint(request CommandRequest) (*os.File, error) {
	file, err := Open(request.Executable, request.SHA256)
	if err != nil {
		return nil, admittedOpenError(err)
	}
	return file, nil
}
