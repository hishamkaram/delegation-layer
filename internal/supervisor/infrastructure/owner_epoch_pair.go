package infrastructure

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// OwnerEpochPair is immutable selection data derived from a successfully
// authenticated owner epoch. It carries no runtime identity, paths, readiness
// proof or native authority; the caller retains its own runtime selection.
type OwnerEpochPair struct {
	ClientSHA256 string
	DaemonSHA256 string
}

func (p OwnerEpochPair) Validate() error {
	if err := errors.Join(task.ValidateSHA256(p.ClientSHA256), task.ValidateSHA256(p.DaemonSHA256)); err != nil {
		return errors.Join(ErrIdentity, err)
	}
	return nil
}

// ProbeOwnerArtifacts observes the selected epoch's pair through the same
// status authentication as ProbeOwner. Only a nil error selects a pair; a
// reference retained alongside a contact error never does. The caller keeps
// its borrowed reader open throughout this observation.
func ProbeOwnerArtifacts(ctx context.Context, store *taskdir.ReadOnlyStore) (StartupReference, OwnerEpochPair, error) {
	observed, err := probeOwnerStatus(ctx, store)
	if err != nil {
		return observed.reference, OwnerEpochPair{}, err
	}
	if err = observed.reference.Validate(); err != nil {
		return observed.reference, OwnerEpochPair{}, err
	}
	pair, err := readOwnerEpochPair(ctx, store, observed)
	return observed.reference, pair, err
}

func readOwnerEpochPair(ctx context.Context, store *taskdir.ReadOnlyStore, observed ownerStatusObservation) (OwnerEpochPair, error) {
	intent, _, publication, err := readOwnerContactRecords(ctx, store, observed.pin, observed.request)
	if err != nil {
		return OwnerEpochPair{}, err
	}
	reader := passiveLifecycleReader{passiveOwnerReader{passiveIntentReader{store}}}
	artifacts, _, err := lifecycleStartupSelection(ctx, reader, observed.pin, intent)
	if err != nil {
		return OwnerEpochPair{}, err
	}
	if err = validateSelectedRuntime(artifacts, publication.RuntimeSHA256); err != nil {
		return OwnerEpochPair{}, err
	}
	pair := OwnerEpochPair{ClientSHA256: artifacts.ClientSHA256, DaemonSHA256: artifacts.DaemonSHA256}
	if err = pair.Validate(); err != nil {
		return OwnerEpochPair{}, err
	}
	// Lifecycle selection cannot retarget the already-authenticated owner.
	if _, _, _, err = readOwnerContactRecords(ctx, store, observed.pin, observed.request); err != nil {
		return OwnerEpochPair{}, err
	}
	return pair, nil
}
