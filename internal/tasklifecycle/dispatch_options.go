package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// TaskStoreOpener supplies the application's predicate composition. The calling
// workflow owns each returned store and closes it at the original boundary.
type TaskStoreOpener func(root string, create bool) (*taskdir.Store, error)

// ContinuationProfiles binds the recorded provider contract to the configured
// candidate factory. Injected Current hooks retain their existing semantics.
type ContinuationProfiles func(task.MetaRecord) admissioncore.Profiles

type DispatchOptions struct {
	Creation             CreationOptions
	Catalog              commonprovider.Catalog
	Workspace            workspaces.OperationOptions
	OpenStore            TaskStoreOpener
	ContinuationProfiles ContinuationProfiles
	ApplyEnvironment     EnvironmentApplicator
}
