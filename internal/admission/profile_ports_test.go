package admission

import (
	"context"
	"errors"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type failedProfileSource struct {
	fault        error
	historical   bool
	currentCalls int
	taskCalls    int
}

func (source *failedProfileSource) Current(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	source.currentCalls++
	return commonprovider.ProfileCandidate{}, source.fault
}

func (source *failedProfileSource) ForTask(task.TaskRecord, *task.MetaRecord) (commonprovider.ProfileCandidate, bool, error) {
	source.taskCalls++
	return commonprovider.ProfileCandidate{}, source.historical, source.fault
}

func TestProfileFactoryFailureDoesNotFallbackOrReadEvidence(t *testing.T) {
	fault := errors.New("selected factory failed")
	for _, historical := range []bool{false, true} {
		for _, worker := range []bool{false, true} {
			source := &failedProfileSource{fault: fault, historical: historical}
			profiles := NewProfiles(source, func(context.Context, string) (*taskdir.Store, error) {
				t.Fatal("factory failure reached evidence reader")
				return nil, task.ErrEvidenceFault
			})
			var err error
			if worker {
				_, err = profiles.ForWorker("", task.TaskRecord{}, nil)
			} else {
				_, _, err = profiles.ExistingContext(t.Context(), "", task.TaskRecord{}, task.MetaRecord{})
			}
			if !errors.Is(err, fault) || source.taskCalls != 1 || source.currentCalls != 0 {
				t.Fatalf("historical=%t worker=%t: err=%v task calls=%d current calls=%d", historical, worker, err, source.taskCalls, source.currentCalls)
			}
		}
	}
}
