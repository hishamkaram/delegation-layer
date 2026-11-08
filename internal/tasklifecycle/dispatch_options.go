package tasklifecycle

import (
	"context"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// TaskStoreOpener supplies the application's predicate composition. The calling
// workflow owns each returned store and closes it at the original boundary.
type TaskStoreOpener func(root string, create bool) (*taskdir.Store, error)

// ContinuationProfiles binds the recorded provider contract to the configured
// candidate factory in the supplied launch scope. Injected Current hooks
// retain their existing semantics.
type ContinuationProfiles func(task.MetaRecord, []string) admissioncore.Profiles

// MemberLookup is the lifecycle's narrow read port; no catalog is opened for a
// request without a Team target or for recovery of an already persisted task.
type MemberLookup func(context.Context, task.TeamTarget, string, string, string) (teamcontract.ResolvedMember, error)

type TeamProjectCheck func(context.Context, task.TeamReference, string, string) error

// DispatchOptions carries one launch scope: Creation.Admission.Profiles owns
// it, and every workflow consumer derives its environment from that value.
type DispatchOptions struct {
	Creation             CreationOptions
	Catalog              commonprovider.Catalog
	Workspace            workspaces.OperationOptions
	OpenStore            TaskStoreOpener
	ContinuationProfiles ContinuationProfiles
	// ResourceGuards selects the invocation-local fresh-admission checks.
	ResourceGuards resourceadmission.GuardPolicy
	// Resources checks fresh ordinary admissions only. Nil selects the native
	// resourceadmission checker when a fresh admission first needs it.
	Resources        ResourceChecker
	Members          MemberLookup
	CheckTeamProject TeamProjectCheck
	// resourceAssessment is the invocation-local assessment a continuation
	// already took for the dispatch it owns, so the dispatch does not sample
	// again. Freshness is still enforced before the task record is written.
	resourceAssessment *resourceadmission.Assessment
}
