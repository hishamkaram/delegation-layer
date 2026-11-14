package gatewaycommand

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

var (
	ErrOfflineCutoverRequired = errors.New("root_cutover_required")
	ErrRootExists             = errors.New("root_exists")
)

// rootContext opens the normal selected Native authority. Creation is an
// exclusive namespace publication; an existing root is never auto-upgraded.
func rootContext(ctx context.Context, rawRoot string, registry predicate.Registry, create bool) (root string, observation taskdir.ConnectorContextObservation, resultErr error) {
	root, err := tasklifecycle.ResolveRoot(rawRoot)
	if err != nil {
		return "", observation, err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, registry)
	if err != nil && create && task.RecordAbsent(err) {
		store, initializeErr := taskdir.InitializeConnectorStoreContext(ctx, root, registry, true)
		if initializeErr != nil {
			if errors.Is(initializeErr, os.ErrExist) {
				return root, observation, errors.Join(ErrRootExists, initializeErr)
			}
			return root, observation, initializeErr
		}
		if closeErr := store.Close(); closeErr != nil {
			return root, observation, closeErr
		}
		reader, err = taskdir.OpenReadOnlyStoreContext(ctx, root, registry)
	}
	if err != nil {
		return root, observation, err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	observation, err = reader.InspectConnectorContext(ctx, "")
	if err != nil {
		return root, observation, err
	}
	for _, feature := range []string{task.NamespaceAdmissionFeature, task.NamespaceMutationFeature, task.WorkspaceConstrainedConnectorFeature, task.PathProjectConnectorFeature} {
		if !slices.Contains(observation.Features, feature) {
			return root, observation, ErrOfflineCutoverRequired
		}
	}
	return root, observation, nil
}

func profileDirectory(root, requested string) (string, error) {
	if requested == "" {
		requested = filepath.Join(root, "gateway")
	}
	profile, err := config.CanonicalizePath(requested)
	if err != nil {
		return "", err
	}
	// Native owns the rest of its root layout. Only the gateway subtree is
	// reserved for connector profiles; a profile cannot contain the task root.
	if config.PathsOverlap(root, profile) {
		relative, relErr := filepath.Rel(root, profile)
		if relErr != nil || (relative != "gateway" && !strings.HasPrefix(relative, "gateway"+string(filepath.Separator))) {
			return "", config.ErrRootWorkspaceOverlap
		}
	}
	return profile, nil
}

func inspectWorkspace(ctx context.Context, root, expectedRoot, directory string, registry predicate.Registry) (canonical, token string, resultErr error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", "", err
	}
	_, canonical, err = config.ValidateDirectories(root, absolute)
	if err != nil {
		return "", "", err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, registry)
	if err != nil {
		return "", "", err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	observation, err := reader.InspectConnectorContext(ctx, "")
	if err != nil {
		return "", "", err
	}
	if observation.RootToken != expectedRoot {
		return "", "", task.ErrIdentityMismatch
	}
	if !slices.Contains(observation.Features, task.PathProjectConnectorFeature) {
		return "", "", taskdir.ErrConnectorDisabled
	}
	identity, err := taskdir.ObservePathProjectContext(ctx, canonical)
	if err != nil {
		return "", "", err
	}
	token, err = taskdir.ProjectToken(identity)
	return canonical, token, err
}
