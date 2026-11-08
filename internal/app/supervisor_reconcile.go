package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func prepareInspectionMigrationSupervisorWithLock(ctx context.Context, store *taskdir.Store, lookup installation.ExecutableLookup, options pueue.Options) (*pueue.Client, error) {
	pending, err := pueue.PrivateSupervisorMigrationPending(store.Root)
	if err != nil {
		return nil, err
	}
	proposals, err := inspection.PreparedGroupMigrationSuccessors(ctx, store)
	if err != nil {
		return nil, err
	}
	if len(proposals) == 0 {
		return prepareCurrentPrivateSupervisor(ctx, store.Root, lookup, options)
	}
	var recorded task.SupervisorRef
	if pending {
		recorded, err = selectPreparedPrivateMigrationTarget(store.Root, proposals)
		if err != nil {
			return nil, err
		}
		retarget, precheckErr := retargetUnavailableInspectionSuccessor(store.Root, recorded)
		if precheckErr != nil {
			return nil, precheckErr
		}
		if retarget {
			client, prepareErr := prepareCurrentPrivateSupervisor(ctx, store.Root, lookup, options)
			if prepareErr != nil {
				return nil, &pueue.PrivateRecoveryError{ReasonCode: "transition_pending", Err: errors.Join(prepareErr, pueue.ErrPrivateSupervisorMigrationRequired)}
			}
			return client, nil
		}
	} else {
		var selection preparedTargetSelection
		recorded, selection, err = selectPreparedTargetBeforePrivateJournalWithLock(store.Root, proposals)
		if err != nil {
			return nil, err
		}
		if selection == preparedTargetUndecided {
			return prepareCurrentPrivateSupervisor(ctx, store.Root, lookup, options)
		}
	}
	client, err := pueue.PrepareRecordedPrivateMigrationWithLock(ctx, store.Root, recorded, options)
	if err != nil {
		return nil, &pueue.PrivateRecoveryError{ReasonCode: "transition_pending", Err: errors.Join(err, pueue.ErrPrivateSupervisorMigrationRequired)}
	}
	return client, nil
}

func selectPreparedPrivateMigrationTarget(root string, proposals []task.SupervisorRef) (task.SupervisorRef, error) {
	var selected task.SupervisorRef
	found := false
	for _, proposal := range proposals {
		matches, err := pueue.PrivateMigrationTargetsWithLock(root, proposal)
		if err != nil {
			return task.SupervisorRef{}, err
		}
		if matches {
			if found {
				return task.SupervisorRef{}, &pueue.PrivateRecoveryError{ReasonCode: "identity_conflict", Err: pueue.ErrPrivateSupervisorMigrationRequired}
			}
			selected, found = proposal, true
		}
	}
	if !found {
		return task.SupervisorRef{}, &pueue.PrivateRecoveryError{ReasonCode: "transition_pending", Err: pueue.ErrPrivateSupervisorMigrationRequired}
	}
	return selected, nil
}

func retargetUnavailableInspectionSuccessor(root string, recorded task.SupervisorRef) (bool, error) {
	missing, err := pueue.PrivateSupervisorPairMissing(recorded)
	if err != nil || !missing {
		return false, err
	}
	return pueue.PrivateMigrationRetargetableWithLock(root)
}

type preparedTargetSelection uint8

const (
	preparedTargetUndecided preparedTargetSelection = iota
	preparedTargetRecorded
)

// Without a private journal, unavailable proposals leave the current resolver
// free to select an installation. Storage and stat faults remain errors.
func selectPreparedTargetBeforePrivateJournalWithLock(root string, proposals []task.SupervisorRef) (task.SupervisorRef, preparedTargetSelection, error) {
	published, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil {
		return task.SupervisorRef{}, preparedTargetUndecided, err
	}
	selected, decision, err := selectPreparedTargetBeforePrivateJournal(proposals, published, found)
	if err != nil || decision == preparedTargetUndecided {
		return selected, decision, err
	}
	missing, err := pueue.PrivateSupervisorPairMissing(selected)
	if err != nil || missing {
		return task.SupervisorRef{}, preparedTargetUndecided, err
	}
	return selected, decision, nil
}

// Proposals alone grant no private shutdown authority. A unique or published
// target selects the recorded pair; ambiguity leaves installation selection to
// the current resolver until the private owner has accepted an edge.
func selectPreparedTargetBeforePrivateJournal(proposals []task.SupervisorRef, published task.SupervisorRef, publishedFound bool) (task.SupervisorRef, preparedTargetSelection, error) {
	unique := proposals[0]
	ambiguous := false
	publishedMatch := false
	for _, proposal := range proposals {
		if err := task.ValidateFreshSupervisorRef(proposal); err != nil {
			return task.SupervisorRef{}, preparedTargetUndecided, err
		}
		ambiguous = ambiguous || proposal != unique
		publishedMatch = publishedMatch || publishedFound && proposal == published
	}
	if !ambiguous {
		return unique, preparedTargetRecorded, nil
	}
	if publishedMatch {
		return published, preparedTargetRecorded, nil
	}
	return task.SupervisorRef{}, preparedTargetUndecided, nil
}
