package infrastructure

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObserveOwnerProcess authenticates the resident and returns its recorded native
// incarnation. Callers must validate that incarnation around placement reads.
func ObserveOwnerProcess(ctx context.Context, reader *taskdir.ReadOnlyStore) (ProcessIdentity, error) {
	reference, err := ProbeOwner(ctx, reader)
	if err != nil {
		return ProcessIdentity{}, err
	}
	request, pin, err := currentOwnerRequestAtStartup(ctx, reader, ownerStatus, reference)
	if err != nil {
		return ProcessIdentity{}, err
	}
	_, assumption, _, err := readOwnerContactRecords(ctx, reader, pin, request)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return assumption.Process, nil
}
