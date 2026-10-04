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

// NativeEnvironment returns nonsecret Claude discovery locations and runtime
// roots. The foreground CCR launcher uses the same Claude configuration paths.
func NativeEnvironment(values []string) ([]string, []string, error) {
	environment, err := prepareNativeEnvironment(values)
	if err != nil {
		return nil, nil, err
	}
	return environment.Values, environment.WritableRoots, nil
}
