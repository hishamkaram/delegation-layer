package inspection

import (
	"encoding/json"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

// Inspect performs the compiled native read and optional policy GET within an
// already-supervised scope. Callers must own the operation's durable permit.
// The scope supplies the existing deadline and final timing gate; no process,
// stop capability, or sensitive output crosses into the provider adapter.
func Inspect(scope execution.PreflightScope, definition provider.InspectionDefinition) (json.RawMessage, error) {
	definition, _, err := definition.Snapshot()
	if err != nil {
		return nil, errNativeInspection
	}
	if scope.Authorize() != nil {
		return nil, errNativeInspection
	}
	if definition.Models != nil {
		return inspectModels(scope, definition)
	}
	var runtimeFacts *provider.RuntimeFacts
	if definition.Runtime != nil {
		facts, runtimeErr := InspectRuntime(scope, *definition.Runtime, definition.OutputLimit)
		if runtimeErr != nil {
			return nil, runtimeErr
		}
		runtimeFacts = &facts
	}
	nativeEnabled := len(definition.Arguments) > 0 || definition.Project != nil || definition.Remote != nil
	var nativeFacts json.RawMessage
	if nativeEnabled {
		digest, digestErr := provider.FingerprintExecutable(definition.Executable)
		if digestErr != nil || digest != definition.ExecutableSHA256 {
			return nil, errNativeInspection
		}
		native, nativeErr := runNative(scope, definition, nativeHooks{})
		if nativeErr != nil {
			return nil, nativeErr
		}
		defer clear(native)
		nativeFacts, err = runProjection(scope.Context(), definition, native)
		if err != nil {
			return nil, err
		}
	}
	if scope.Authorize() != nil {
		return nil, errNativeInspection
	}
	if runtimeFacts != nil {
		return provider.EncodeInspectionFacts(*runtimeFacts, nativeFacts)
	}
	return nativeFacts, nil
}
