package workspace

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/internal/journal"
)

type (
	Resource         = journal.Resource
	Operation        = journal.Operation
	SubmissionPermit = journal.SubmissionPermit
)

// ErrCheckpointRemovalHeadChanged reports a lost removal registration race.
var ErrCheckpointRemovalHeadChanged = journal.ErrCheckpointRemovalHeadChanged

func OpenOperation(ctx context.Context, store *taskdir.Store, request OperationRequest) (*Operation, error) {
	return journal.OpenOperation(ctx, store, request)
}

func LoadOperation(ctx context.Context, store *taskdir.Store, id string) (*Operation, error) {
	return journal.LoadOperation(ctx, store, id)
}

func ObserveLifetimeContext(ctx context.Context, store *taskdir.Store, id string, observe func(taskdir.ControlLifetimeObservation) error) error {
	return journal.ObserveLifetimeContext(ctx, store, id, observe)
}

// OperationRequestPresent probes one existing operation directory for its
// request record without locking, creating or decoding it.
func OperationRequestPresent(ctx context.Context, store *taskdir.Store, id string) (bool, error) {
	return journal.OperationRequestPresent(ctx, store, id)
}

func EstablishResource(ctx context.Context, store *taskdir.Store, request ResourceRequest) (*Resource, error) {
	return journal.EstablishResource(ctx, store, request)
}

func LoadResource(ctx context.Context, store *taskdir.Store, id string) (*Resource, error) {
	return journal.LoadResource(ctx, store, id)
}

func ReadResourceRequest(ctx context.Context, reader *taskdir.ReadOnlyStore, id string) (ResourceRequest, error) {
	return journal.ReadResourceRequest(ctx, reader, id)
}

func ResourceIDs(ctx context.Context, store *taskdir.Store) ([]string, error) {
	return journal.ResourceIDs(ctx, store)
}
func ContainerPath(root, rootID string) string    { return journal.ContainerPath(root, rootID) }
func ScratchPath(root, operationID string) string { return journal.ScratchPath(root, operationID) }
func CapturePurpose(sequence uint64) string       { return journal.CapturePurpose(sequence) }
func OperationAbsent(err error) bool              { return journal.OperationAbsent(err) }
