package admission

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The immutable request owns historical definition data. Reconstruct its exact
// snapshot without reading a program, preserving every other definition field.
func inspectionDefinitionForBinding(definition commonprovider.InspectionDefinition, saved inspection.Binding) (commonprovider.InspectionDefinition, string, error) {
	if saved.ExecutableIdentity == "" && definition.ExecutableSHA256 == "" && !definition.ProviderExecutable() && definition.Executable == saved.HelperExecutable {
		definition.ExecutableSHA256 = saved.HelperSHA256
	}
	return definition.Snapshot()
}

func inspectionHelperDigestMatches(saved inspection.Binding, definition commonprovider.InspectionDefinition) bool {
	return saved.ExecutableIdentity == task.ExecutableIdentityInstalledPathV1 || saved.HelperSHA256 == definition.ExecutableSHA256
}

// This derived replay view never becomes a new request. The canonical request
// retains its original marker and digests; selected worker changes still pass
// the existing managed-worker authorization before that request can be reused.
func inspectionReplayBinding(ctx context.Context, store *taskdir.Store, request task.TaskRecord, candidate commonprovider.ProfileCandidate, current inspection.Binding) (binding inspection.Binding, resultErr error) {
	operation, err := LoadInspectionOperationContext(ctx, store, request)
	if err != nil {
		if task.RecordAbsent(err) {
			return current, nil
		}
		return inspection.Binding{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err = ValidateInspectionCandidate(ctx, operation, request, candidate); err != nil {
		return inspection.Binding{}, err
	}
	saved := operation.Request().Binding
	current.DefinitionSHA256 = saved.DefinitionSHA256
	if saved.ExecutableIdentity == "" {
		current.ExecutableIdentity = ""
		current.HelperSHA256 = saved.HelperSHA256
		current.WorkerSHA256 = saved.WorkerSHA256
	}
	return current, nil
}
