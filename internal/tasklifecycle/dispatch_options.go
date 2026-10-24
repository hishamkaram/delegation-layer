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
// candidate factory in the supplied launch scope. Injected Current hooks
// retain their existing semantics.
type ContinuationProfiles func(task.MetaRecord, []string) admissioncore.Profiles

// DispatchOptions carries one launch scope: Creation.Admission.Profiles owns
// it, and every workflow consumer derives its environment from that value.
type DispatchOptions struct {
	Creation             CreationOptions
	Catalog              commonprovider.Catalog
	Workspace            workspaces.OperationOptions
	OpenStore            TaskStoreOpener
	ContinuationProfiles ContinuationProfiles
}
