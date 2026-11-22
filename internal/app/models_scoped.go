package app

import (
	"context"
	"errors"
	"flag"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ScopedModelArguments select one approved path for advisory discovery. The
// binding is derived by Native before any inspection or supervisor is created.
type ScopedModelArguments struct {
	ExpectedRoot  string
	WorkspaceFile string
	RelativePath  string
	binding       *task.ConnectorContext
}

func scopedModelCustody(ctx context.Context, store *taskdir.Store, record inspection.RequestRecord) (execution.ProjectDirectory, error) {
	if record.Binding.DefinitionRevision != commonprovider.ModelsRevision || record.Task.ConnectorContext == nil {
		return nil, nil
	}
	project, err := store.NewPathProjectCustody(ctx, *record.Task.ConnectorContext)
	if err != nil {
		return nil, err
	}
	return project, nil
}

func (a ScopedModelArguments) requested() bool {
	return a.ExpectedRoot != "" || a.WorkspaceFile != "" || a.RelativePath != ""
}

func registerScopedModelFlags(fs *flag.FlagSet, a *Arguments) {
	fs.StringVar(&a.Models.ExpectedRoot, "expected-root", "", "expected opaque root context token")
	fs.StringVar(&a.Models.WorkspaceFile, "expected-workspace-file", "", "absolute regular file containing the exact approved path workspace token")
	fs.StringVar(&a.Models.RelativePath, "relative-project", "", "project selection beneath the approved workspace")
}

func validateScopedModelArguments(a Arguments) error {
	if !a.Models.requested() {
		return nil
	}
	if err := task.ValidateSHA256(a.Models.ExpectedRoot); err != nil {
		return errors.New("scoped models requires --expected-root")
	}
	if !filepath.IsAbs(a.Models.WorkspaceFile) || a.Models.RelativePath == "" {
		return errors.New("scoped models requires --expected-workspace-file ABS and --relative-project")
	}
	return nil
}

func prepareScopedModels(ctx context.Context, a *Arguments, root string, deps Dependencies) (resultErr error) {
	if !a.Models.requested() {
		return nil
	}
	file, err := openConnectorJSONFile(a.Models.WorkspaceFile)
	if err != nil {
		return err
	}
	data, readErr := task.ReadBounded(connectorJSONReader{ctx: ctx, input: file}, task.MaxControlRecordSize)
	if err = errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	constraint := &task.WorkspaceConstraint{Token: string(data), RelativeProject: a.Models.RelativePath}
	if err = constraint.Validate(); err != nil {
		return err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	observation, err := reader.InspectWorkspaceConnectorContext(ctx, constraint)
	if err != nil {
		return err
	}
	if observation.RootToken != a.Models.ExpectedRoot {
		return task.ErrIdentityMismatch
	}
	binding, err := reader.RequireWorkspaceConnectorContext(ctx, a.Models.ExpectedRoot, observation.ProjectToken, constraint)
	if err != nil {
		return err
	}
	if !binding.Project.IsPath() || (a.Cwd != "" && a.Cwd != binding.Project.Path) {
		return task.ErrIdentityMismatch
	}
	a.Cwd, a.Models.binding = binding.Project.Path, binding.Clone()
	return nil
}

func openModelsStore(ctx context.Context, a Arguments, root string, deps Dependencies) (*taskdir.Store, *taskdir.PathProjectCustody, error) {
	if a.Models.binding == nil {
		store, err := openStoreContext(ctx, root, deps.storeDependencies(), true)
		return store, nil, err
	}
	// The expected root is checked before any filesystem support probe writes.
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return nil, nil, err
	}
	custody, err := store.NewPathProjectCustody(ctx, *a.Models.binding)
	if err == nil && deps.remoteRootPin != nil {
		var pin taskdir.RootPin
		pin, err = store.PinRootContext(ctx)
		if err == nil && !pin.Equal(*deps.remoteRootPin) {
			err = task.ErrIdentityMismatch
		}
	}
	if err == nil {
		err = store.ProbeFilesystemSupportContext(ctx)
	}
	if err != nil {
		return nil, nil, errors.Join(err, store.Close())
	}
	return store, custody, nil
}
