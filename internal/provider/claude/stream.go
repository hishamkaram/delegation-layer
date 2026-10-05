package claude

import (
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NewStreamInterpreter reuses the native Claude stream protocol for a caller-owned
// adapter contract. Preparation, predicate identity, and session ownership stay
// with that adapter; no task or evidence is relabelled as claude:print.
func NewStreamInterpreter(reference task.PredicateRef) (predicate.Interpreter, error) {
	if err := task.ValidatePredicateRef(reference); err != nil {
		return nil, err
	}
	if reference.Mode != Mode && reference.Mode != WorkspaceWriteMode {
		return nil, fmt.Errorf("unsupported Claude stream permission mode %q", reference.Mode)
	}
	return interpreter{mode: reference.Mode, streamReference: &reference}, nil
}

// NewStreamInterpreterWithRecovery binds a caller-owned current stream
// interpreter to its explicit same-wire historical source reference. The
// source is used only by offline recovery; ordinary evaluation still requires
// the current reference and recovery never rewrites the sealed input.
func NewStreamInterpreterWithRecovery(reference, recoverySource task.PredicateRef) (predicate.Interpreter, error) {
	if err := task.ValidatePredicateRef(reference); err != nil {
		return nil, err
	}
	if err := task.ValidatePredicateRef(recoverySource); err != nil {
		return nil, err
	}
	if reference.Mode != Mode && reference.Mode != WorkspaceWriteMode || recoverySource.Mode != reference.Mode {
		return nil, fmt.Errorf("unsupported Claude stream permission mode %q", reference.Mode)
	}
	return interpreter{mode: reference.Mode, streamReference: &reference, recoverySource: &recoverySource}, nil
}

// NewHistoricalStreamInterpreter replays the pre-issue47 current native
// Claude parser under a caller-owned adapter reference. It is intentionally
// separate from NewStreamInterpreter so fresh tasks always bind the corrected
// offset-streaming parser.
func NewHistoricalStreamInterpreter(reference task.PredicateRef) (predicate.Interpreter, error) {
	if err := task.ValidatePredicateRef(reference); err != nil {
		return nil, err
	}
	if reference.Mode != Mode && reference.Mode != WorkspaceWriteMode {
		return nil, fmt.Errorf("unsupported Claude stream permission mode %q", reference.Mode)
	}
	return interpreter{mode: reference.Mode, streamReference: &reference, historicalNativeCurrent: true}, nil
}

// NativeEnvironment returns nonsecret Claude discovery locations and runtime
// roots. The foreground CCR launcher uses the same Claude configuration paths.
func NativeEnvironment(values []string) ([]string, []string, error) {
	environment, err := prepareNativeEnvironment(values)
	if err != nil {
		return nil, nil, err
	}
	return environment.Values, environment.WritableRoots, nil
}
