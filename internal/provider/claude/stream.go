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
// reference to the frozen advisory completion policy described by
// AdvisoryNativeContractForMode: the caller's principal result is the answer, root
// provider-native background notification cycles must be attributed and
// settled before EOF, nested agent messages stay outside those cycles, only
// session identity and native permission are init authority, and an unknown
// completion reason is unsupported. The listed same-wire historical sources
// are used only by offline recovery; ordinary evaluation still requires the
// bound reference and recovery never rewrites the sealed input.
func NewAttributedStreamInterpreterWithRecovery(reference task.PredicateRef, recoverySources ...task.PredicateRef) (predicate.Interpreter, error) {
	return newAttributedStreamInterpreter(reference, advisoryScopedCompletion, recoverySources)
}

func newAttributedStreamInterpreter(reference task.PredicateRef, policy completionPolicy, recoverySources []task.PredicateRef) (predicate.Interpreter, error) {
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
	return interpreter{mode: reference.Mode, streamReference: &reference, recoverySources: slices.Clone(recoverySources), completion: policy}, nil
}

// NewFollowUpStreamInterpreterWithRecovery binds a caller-owned stream
// reference to the current follow-up completion policy described by
// ContractForMode: the principal and all accepted root follow-up results are the answer, root
// provider-native background notification cycles must be attributed and
// settled before EOF, nested agent messages stay outside those cycles, only
// session identity and native permission are init authority, and an unknown
// completion reason is unsupported. The listed same-wire historical sources
// are used only by offline recovery; ordinary evaluation still requires the
// bound reference and recovery never rewrites the sealed input.
func NewFollowUpStreamInterpreterWithRecovery(reference task.PredicateRef, recoverySources ...task.PredicateRef) (predicate.Interpreter, error) {
	return newAttributedStreamInterpreter(reference, advisoryFollowUpCompletion, recoverySources)
}

// NewScopedStreamInterpreter replays a caller-owned stream reference sealed
// under the frozen scoped policy and strict wire described by
// ScopedNativeContractForMode. It is replay-only and never a recovery target.
func NewScopedStreamInterpreter(reference task.PredicateRef) (predicate.Interpreter, error) {
	if err := task.ValidatePredicateRef(reference); err != nil {
		return nil, err
	}
	if reference.Mode != Mode && reference.Mode != WorkspaceWriteMode {
		return nil, fmt.Errorf("unsupported Claude stream permission mode %q", reference.Mode)
	}
	return interpreter{mode: reference.Mode, streamReference: &reference, completion: scopedAttributedCompletion}, nil
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
