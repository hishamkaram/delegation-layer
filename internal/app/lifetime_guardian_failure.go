package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// lifetimeGuardianCauseLimit bounds how many concrete causes one diagnostic names.
const lifetimeGuardianCauseLimit = 16

// writeLifetimeGuardianFailure keeps the public guardian error and exit code
// generic and records the returned causes on one private operational line for
// the supervisor's captured log. Error text never reaches that line: wrapped
// messages may carry paths, arguments, or credentials. Only code-owned
// identities do.
func writeLifetimeGuardianFailure(stderr io.Writer, cause error) int {
	diagnostic := "delegate: lifetime guardian failed: causes=" + strings.Join(lifetimeGuardianCauseIdentities(cause), ",")
	if err := log.New(stderr, "", log.LstdFlags).Output(1, diagnostic); err != nil {
		return 1
	}
	return writeCLIError(stderr, errors.New("task lifetime unavailable"), 1)
}

// lifetimeGuardianCauseIdentities names every concrete cause in the error
// tree, depth first, so joined faults stay visible. A cause is named by its
// exported sentinel, its errno, or, when unknown, only its Go type.
func lifetimeGuardianCauseIdentities(cause error) []string {
	identities := make([]string, 0, lifetimeGuardianCauseLimit)
	pending := []error{cause}
	omitted := 0
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			continue
		}
		if children := lifetimeGuardianCauseChildren(current); len(children) > 0 {
			// Concat copies: Unwrap may return the wrapper's own slice.
			pending = slices.Concat(children, pending)
			continue
		}
		if len(identities) == lifetimeGuardianCauseLimit {
			omitted++
			continue
		}
		identities = append(identities, lifetimeGuardianLeafIdentity(current))
	}
	if omitted > 0 {
		identities = append(identities, fmt.Sprintf("omitted:%d", omitted))
	}
	return identities
}

// lifetimeGuardianCauseChildren returns the errors a node wraps; a leaf
// returns none.
func lifetimeGuardianCauseChildren(node error) []error {
	if joined, ok := node.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	if child := errors.Unwrap(node); child != nil {
		return []error{child}
	}
	return nil
}

// lifetimeGuardianLeafIdentity names one leaf by identity, never by message.
func lifetimeGuardianLeafIdentity(leaf error) string {
	for _, sentinel := range lifetimeGuardianSentinels() {
		if errors.Is(leaf, sentinel.err) {
			return sentinel.name
		}
	}
	var errno syscall.Errno
	if errors.As(leaf, &errno) {
		return fmt.Sprintf("errno:%d", uintptr(errno))
	}
	return fmt.Sprintf("unknown:%T", leaf)
}

type lifetimeGuardianSentinel struct {
	name string
	err  error
}

// lifetimeGuardianSentinels lists the exported sentinels a guardian run can
// return, with their code names.
func lifetimeGuardianSentinels() []lifetimeGuardianSentinel {
	return []lifetimeGuardianSentinel{
		{"task.ErrEvidenceFault", task.ErrEvidenceFault},
		{"task.ErrIdentityMismatch", task.ErrIdentityMismatch},
		{"task.ErrInvariantFault", task.ErrInvariantFault},
		{"task.ErrUncertainDurability", task.ErrUncertainDurability},
		{"task.ErrLockBusy", task.ErrLockBusy},
		{"task.ErrTerminalTask", task.ErrTerminalTask},
		{"task.ErrInvalidTaskID", task.ErrInvalidTaskID},
		{"task.ErrInvalidRootID", task.ErrInvalidRootID},
		{"task.ErrUnsupportedSchema", task.ErrUnsupportedSchema},
		{"task.ErrDuplicateKey", task.ErrDuplicateKey},
		{"task.ErrTrailingJSON", task.ErrTrailingJSON},
		{"task.ErrControlRecordTooBig", task.ErrControlRecordTooBig},
		{"task.ErrInvalidEnum", task.ErrInvalidEnum},
		{"task.ErrLifetimeUnlaunched", task.ErrLifetimeUnlaunched},
		{"lifetime.ErrInvalidRef", lifetime.ErrInvalidRef},
		{"lifetime.ErrInvalidLaunchRef", lifetime.ErrInvalidLaunchRef},
		{"lifetime.ErrUnsupported", lifetime.ErrUnsupported},
		{"lifetime.ErrClosed", lifetime.ErrClosed},
		{"lifetime.ErrScopeAbsent", lifetime.ErrScopeAbsent},
		{"lifetime.ErrScopeExists", lifetime.ErrScopeExists},
		{"lifetime.ErrNilContext", lifetime.ErrNilContext},
		{"lifetime.ErrNotEmpty", lifetime.ErrNotEmpty},
		{"lifetime.ErrConflictingSysProc", lifetime.ErrConflictingSysProc},
		{"lifetime.ErrUnverifiableLaunch", lifetime.ErrUnverifiableLaunch},
		{"guardian.ErrProofPending", guardian.ErrProofPending},
		{"guardian.ErrRecoveryPending", guardian.ErrRecoveryPending},
		{"taskdir.ErrUnsupportedFilesystem", taskdir.ErrUnsupportedFilesystem},
		{"taskdir.ErrLockOrderViolation", taskdir.ErrLockOrderViolation},
		{"taskdir.ErrLockNotHeld", taskdir.ErrLockNotHeld},
		{"verifiedexec.ErrPoolNoExec", verifiedexec.ErrPoolNoExec},
		{"verifiedexec.ErrAdmittedExecutableChanged", verifiedexec.ErrAdmittedExecutableChanged},
		{"context.Canceled", context.Canceled},
		{"context.DeadlineExceeded", context.DeadlineExceeded},
		{"os.ErrClosed", os.ErrClosed},
		{"io.EOF", io.EOF},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF},
	}
}
