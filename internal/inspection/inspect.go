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
	if err != nil || scope.Authorize() != nil {
		return nil, errNativeInspection
	}
	digest, err := provider.FingerprintExecutable(definition.Executable)
	if err != nil || digest != definition.ExecutableSHA256 {
		return nil, errNativeInspection
	}
	native, err := runNative(scope.Context(), definition, scope.Authorize, nativeHooks{})
	if err != nil {
		return nil, err
	}
	defer clear(native)
	facts, err := runProjection(scope.Context(), definition, native)
	if err != nil {
		return nil, err
	}
	if scope.Authorize() != nil {
		return nil, errNativeInspection
	}
	return facts, nil
}
