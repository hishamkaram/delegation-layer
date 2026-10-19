package claude

import (
	"fmt"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NewStreamInterpreter reuses the native Claude stream protocol for a caller-owned
// adapter contract under the frozen single-result completion policy. It is
// replay-only and never an offline recovery target. Preparation, predicate
// identity, and session ownership stay with that adapter; no task or evidence
// is relabelled as claude:print.
func NewStreamInterpreter(reference task.PredicateRef) (predicate.Interpreter, error) {
	if err := task.ValidatePredicateRef(reference); err != nil {
		return nil, err
	}
	if reference.Mode != Mode && reference.Mode != WorkspaceWriteMode {
		return nil, fmt.Errorf("unsupported Claude stream permission mode %q", reference.Mode)
	}
	return interpreter{mode: reference.Mode, streamReference: &reference}, nil
}

// NewAttributedStreamInterpreterWithRecovery binds a caller-owned stream
// reference to the scoped attributed completion policy described by
// ContractForMode: the caller's principal result is the answer, root
// provider-native background notification cycles must be attributed and
// settled before EOF, and nested agent messages stay outside those cycles. The listed
// same-wire historical sources are used only by offline recovery; ordinary
// evaluation still requires the bound reference and recovery never rewrites
// the sealed input.
func NewAttributedStreamInterpreterWithRecovery(reference task.PredicateRef, recoverySources ...task.PredicateRef) (predicate.Interpreter, error) {
	if err := task.ValidatePredicateRef(reference); err != nil {
		return nil, err
	}
	if reference.Mode != Mode && reference.Mode != WorkspaceWriteMode {
		return nil, fmt.Errorf("unsupported Claude stream permission mode %q", reference.Mode)
	}
	for _, source := range recoverySources {
		if err := task.ValidatePredicateRef(source); err != nil {
			return nil, err
		}
		if source.Mode != reference.Mode || source.Equal(reference) {
			return nil, fmt.Errorf("invalid Claude stream recovery source %s/%s", source.Adapter, source.Version)
		}
	}
	return interpreter{mode: reference.Mode, streamReference: &reference, recoverySources: slices.Clone(recoverySources), completion: scopedAttributedCompletion}, nil
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
