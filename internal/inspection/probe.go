package inspection

import (
	"encoding/json"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	inspectionprobe "github.com/hishamkaram/delegation-layer/internal/inspection/internal/probe"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

func Inspect(scope execution.PreflightScope, definition provider.InspectionDefinition) (json.RawMessage, error) {
	return inspectionprobe.Inspect(scope, definition)
}

func InspectRuntime(scope execution.PreflightScope, definition provider.RuntimeProbeDefinition, outputLimit int64) (provider.RuntimeFacts, error) {
	return inspectionprobe.InspectRuntime(scope, definition, outputLimit)
}
