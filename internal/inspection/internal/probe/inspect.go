package probe

import (
	"context"
	"encoding/json"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
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
		native, nativeErr := runNative(scope, definition, nativeHooks{prepareEnvironment: scope.PrepareEnvironment})
		if nativeErr != nil {
			return nil, nativeErr
		}
		defer clear(native)
		projectionCtx, cancelProjection := nativeProjectionContext(scope.Context())
		defer cancelProjection()
		nativeFacts, err = runProjection(projectionCtx, definition, native)
		if err != nil {
			return nil, err
		}
	}
	if scope.Authorize() != nil {
		return nil, errNativeInspection
	}
	return encodeInspectionFacts(definition, runtimeFacts, nativeFacts)
}

// Native commands retain their task's supervision. Projection may perform a
// network read, so unlimited execution supplies the existing finite auxiliary
// inspection bound without adding an execution timer to the parent task.
func nativeProjectionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if execution.IsUnlimitedExecutionContext(ctx) {
		return context.WithTimeout(ctx, contract.AdmissionTimeout)
	}
	return ctx, func() {}
}

func encodeInspectionFacts(definition provider.InspectionDefinition, runtimeFacts *provider.RuntimeFacts, nativeFacts json.RawMessage) (json.RawMessage, error) {
	if runtimeFacts == nil {
		return nativeFacts, nil
	}
	capabilitySHA, err := provider.RuntimeCapabilityDigest(definition)
	if err != nil {
		return nil, errNativeInspection
	}
	runtimeFacts.CapabilitySHA256 = capabilitySHA
	return provider.EncodeInspectionFacts(*runtimeFacts, nativeFacts)
}
