package workspacefiles

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Execute owns one bounded observation. nativeRoot is selected by the Native
// application, not by the remote caller. Root validation happens before scope
// acquisition and before output release. Only pure OS scope discovery can omit
// both nativeRoot and ExpectedRoot. A caller of macOS scopes must supply a finite
// context for the existing Native process owner used by the fixed OS lookup.
func Execute(ctx context.Context, nativeRoot string, operation Operation, request Request) (result Result, resultErr error) {
	path, err := validateRequest(ctx, operation, request)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		result, resultErr = finishObservation(ctx, result, resultErr)
	}()
	reader, err := openRootGuard(ctx, nativeRoot, operation, request)
	if err != nil {
		return Result{}, err
	}
	if reader != nil {
		defer func() {
			guardErr := validateRoot(ctx, reader, request.ExpectedRoot)
			resultErr = errors.Join(resultErr, guardErr, classify("root_close", reader.Close()))
		}()
	}
	result = Result{SchemaVersion: SchemaVersion, Operation: operation, ObservedAt: time.Now().UTC()}
	if operation == Scopes {
		result.Scopes, resultErr = observeScopes(ctx, request.Scope)
		return result, resultErr
	}
	selected, err := openScope(ctx, request.Scope)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, classify("scope_close", selected.Close()), classify("context", ctx.Err()))
	}()
	switch operation {
	case List:
		result.List, resultErr = selected.list(ctx, path, request)
	case Read:
		result.Read, resultErr = selected.read(ctx, path, request)
	case Inspect, Git:
		result.Inspect, resultErr = selected.inspect(ctx, path, request.ExpectedVersion)
		if resultErr == nil && operation == Git && result.Inspect.Entry.Kind != Directory {
			resultErr = failure(NotDirectory, "git_inspect", nil)
		}
	case Scopes:
		return Result{}, failure(InvalidRequest, "operation", nil)
	}
	return result, resultErr
}

// A failed, canceled or oversized observation releases no partial result.
func finishObservation(ctx context.Context, result Result, resultErr error) (Result, error) {
	if resultErr != nil {
		return Result{}, resultErr
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > MaxResponseBytes {
		return Result{}, failure(ResourceLimit, "response_encoding", err)
	}
	if err = ctx.Err(); err != nil {
		return Result{}, classify("context", err)
	}
	return result, nil
}

func openRootGuard(ctx context.Context, nativeRoot string, operation Operation, request Request) (*taskdir.ReadOnlyStore, error) {
	if operation == Scopes && request.Scope.Kind == "" && nativeRoot == "" && request.ExpectedRoot == "" {
		return nil, nil
	}
	if nativeRoot == "" || request.ExpectedRoot == "" {
		return nil, failure(InvalidRequest, "root_guard", nil)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, nativeRoot, predicate.Default())
	if err != nil {
		return nil, classify("root_guard", err)
	}
	if err = validateRoot(ctx, reader, request.ExpectedRoot); err != nil {
		return nil, errors.Join(err, classify("root_close", reader.Close()))
	}
	return reader, nil
}

func validateRoot(ctx context.Context, reader *taskdir.ReadOnlyStore, expected string) error {
	observation, err := reader.InspectConnectorContext(ctx, "")
	if err != nil {
		return classify("root_guard", err)
	}
	if observation.RootToken != expected {
		return failure(Changed, "root_guard", nil)
	}
	return nil
}
