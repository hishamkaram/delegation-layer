package admission

import (
	"context"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// CandidateSource supplies the configured provider factories. ForTask reports
// whether the candidate came from the historical factory. A nil metadata input
// selects the current factory; any factory error ends preparation.
// Admission owns all validation and evidence reads after factory selection.
type CandidateSource interface {
	Current(task.TaskRecord) (commonprovider.ProfileCandidate, error)
	ForTask(task.TaskRecord, *task.MetaRecord) (commonprovider.ProfileCandidate, bool, error)
}

// ProfileStoreOpener retains the application's exact predicate interpretation.
// The profile workflow owns the timing and closure of every store it opens.
type ProfileStoreOpener func(context.Context, string) (*taskdir.Store, error)
