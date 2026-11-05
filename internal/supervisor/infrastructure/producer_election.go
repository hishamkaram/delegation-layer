package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func (a *StartupAttempt) joinBusy(ctx context.Context, request EstablishmentRequest) error {
	latest, found, err := readLatestIntent(ctx, a.reader)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: occupied infrastructure instance has no startup reference", task.ErrLockBusy)
	}
	reader := passiveLifecycleReader{passiveOwnerReader{passiveIntentReader{a.reader}}}
	if err = a.requireOrdinaryBirth(ctx, reader); err != nil {
		return err
	}
	if err = validateLifecycleStartupHistory(ctx, reader, a.pin, latest); err != nil {
		return err
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return err
	}
	if err = validateCurrentIntent(latest, a.pin, boot); err != nil {
		return err
	}
	evidence, err := a.requireJoinable(ctx, latest)
	if err != nil {
		return err
	}
	if latest.Inputs.Kind == StartupUpgrade || evidence.publication != nil {
		return a.requireOwnerContact(ctx, reader, latest, request.Requirements, evidence)
	}
	if err = a.matchPendingRootOwner(ctx, request, latest); err != nil {
		return err
	}
	a.intent = latest
	return nil
}

func (a *StartupAttempt) matchPendingRootOwner(ctx context.Context, request EstablishmentRequest, intent StartupIntent) error {
	boot, err := lifetime.BootID()
	if err != nil {
		return err
	}
	if err = validateCurrentIntent(intent, a.pin, boot); err != nil {
		return err
	}
	if intent.Inputs.Kind != StartupRootOwner {
		return ErrIncompatible
	}
	if err = request.Requirements.ValidateArtifacts(intent.Inputs.Artifacts); err != nil {
		return err
	}
	digest, err := request.Context.Digest()
	if err != nil {
		return err
	}
	if intent.Inputs.Context.SHA256 != digest || intent.Inputs.Initial != request.Initial.normalized() {
		return task.ErrLockBusy
	}
	return a.checkRoot(ctx)
}

func (a *StartupAttempt) elect(ctx context.Context, request EstablishmentRequest, source capturedEstablishment, native producerNative) error {
	var err error
	a.journal, err = openIntentJournal(ctx, a.target, true)
	if err != nil {
		return err
	}
	if err = a.requireOrdinaryBirth(ctx, a.currentLifecycleReader()); err != nil {
		return err
	}
	latest, found, err := a.journal.latestIntent(ctx)
	if err != nil {
		return err
	}
	if found {
		pending, checkErr := a.prepareSuccessor(ctx, request, latest)
		if checkErr != nil {
			return checkErr
		}
		if pending {
			a.intent = latest
			return a.releaseElection()
		}
	}
	if !time.Now().Before(request.Deadline) {
		return context.DeadlineExceeded
	}
	artifacts, err := source.prepare(ctx, a.target)
	if err != nil {
		return err
	}
	if err = request.Requirements.ValidateArtifacts(artifacts); err != nil {
		return err
	}
	if err = artifacts.validate(); err != nil {
		return err
	}
	if err = a.checkRoot(ctx); err != nil {
		return err
	}
	reference, _, err := a.journal.stageContext(ctx, request.Context)
	if err != nil {
		return err
	}
	intent, err := a.newIntent(artifacts, reference, request, latest.Sequence, source.starter)
	if err != nil {
		return err
	}
	return a.acceptAndStart(ctx, intent, source.starter.environment, native)
}

// A future-deadline unassumed attempt is joined, even with a free lease. An
// assumed same-boot live incarnation faults under the canonical settler. No
// lifecycle abandonment/upgrade permission is inferred from a free descriptor.
func (a *StartupAttempt) prepareSuccessor(ctx context.Context, request EstablishmentRequest, latest StartupIntent) (bool, error) {
	if err := validateLifecycleStartupHistory(ctx, a.currentLifecycleReader(), a.pin, latest); err != nil {
		return false, err
	}
	if err := a.requireSettledHistory(ctx, latest.Sequence); err != nil {
		return false, err
	}
	reference, err := ReferenceStartup(latest)
	if err != nil {
		return false, err
	}
	evidence, err := loadSettlementEvidence(ctx, controlOwnerReader{controlIntentReader{a.journal.control}}, a.pin, reference)
	if err != nil {
		return false, err
	}
	if _, readErr := a.journal.readSettlement(ctx, reference); readErr == nil {
		_, _, settleErr := a.journal.settleStartup(ctx, a.lease, reference)
		return false, settleErr
	} else if !task.RecordAbsent(readErr) {
		return false, readErr
	}
	pending, err := unassumedStartupPending(latest, evidence.assumption)
	if err != nil {
		return false, err
	}
	if pending {
		if latest.Inputs.Kind == StartupUpgrade {
			return false, task.ErrLockBusy
		}
		if err = a.matchPendingRootOwner(ctx, request, latest); err != nil {
			return false, err
		}
		return true, nil
	}
	if !time.Now().Before(request.Deadline) {
		return false, context.DeadlineExceeded
	}
	_, _, err = a.journal.settleStartup(ctx, a.lease, reference)
	return false, err
}

// Prior immutable attempts must already have canonical settlements. Only the
// latest may be newly settled, so stranded legacy gaps cannot be erased by
// electing a new head. Every same-boot assumed incarnation is freshly observed.
func (a *StartupAttempt) requireSettledHistory(ctx context.Context, latest uint64) error {
	names, err := a.journal.control.RecordNamesContext(ctx)
	if err != nil {
		return err
	}
	sequences := []uint64{}
	for _, name := range names {
		if len(name) < 7 || name[:7] != "intent-" {
			continue
		}
		sequence, parseErr := intentSequence(name)
		if parseErr != nil {
			return parseErr
		}
		sequences = append(sequences, sequence)
	}
	slices.Sort(sequences)
	if uint64(len(sequences)) != latest {
		return task.ErrEvidenceFault
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return err
	}
	for index, sequence := range sequences {
		if sequence != uint64(index)+1 {
			return task.ErrEvidenceFault
		}
		if sequence == latest {
			continue
		}
		if err = a.checkPriorSettlement(ctx, sequence, boot); err != nil {
			return err
		}
	}
	return a.checkRoot(ctx)
}

func (a *StartupAttempt) checkPriorSettlement(ctx context.Context, sequence uint64, boot string) error {
	intent, err := a.journal.readIntent(ctx, sequence)
	if err != nil {
		return err
	}
	reference, err := ReferenceStartup(intent)
	if err != nil {
		return err
	}
	return validateSettledLifecycleStartup(ctx, a.currentLifecycleReader(), a.pin, boot, reference)
}

func (a *StartupAttempt) newIntent(artifacts Artifacts, launch LaunchContextReference, request EstablishmentRequest, last uint64, starter capturedProducerStarter) (StartupIntent, error) {
	if last == ^uint64(0) {
		return StartupIntent{}, ErrIdentity
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return StartupIntent{}, err
	}
	epoch, err := randomid.NewTaskID()
	if err != nil {
		return StartupIntent{}, err
	}
	inputs := StartupInputs{Kind: StartupRootOwner, Initial: request.Initial, Root: a.pin.Root(), RootID: a.pin.RootID(), RootDevice: a.pin.Device(), RootInode: a.pin.Inode(), BootID: boot, Artifacts: artifacts, Context: launch}
	digest, err := inputs.Digest()
	if err != nil {
		return StartupIntent{}, err
	}
	provenance := starter.provenance
	if provenance.Kind == StarterOutside {
		provenance.Root, provenance.RootID = a.pin.Root(), a.pin.RootID()
	}
	protocol := ProtocolVersion
	if inputs.Initial != (InitialOperation{}) {
		protocol = startupInitialOperationProtocolVersion
	}
	intent := StartupIntent{Protocol: protocol, Sequence: last + 1, Epoch: epoch, Inputs: inputs, InputsSHA256: digest, Starter: provenance, AcceptedAt: time.Now().UTC().Format(time.RFC3339Nano), Deadline: request.Deadline.UTC().Format(time.RFC3339Nano)}
	return intent, intent.Validate()
}

// Busy observers read canonical evidence only; an occupied descriptor never
// revives a settled or expired unassumed attempt and grants no fresh Start.
func (a *StartupAttempt) requireJoinable(ctx context.Context, intent StartupIntent) (settlementEvidence, error) {
	reference, err := ReferenceStartup(intent)
	if err != nil {
		return settlementEvidence{}, err
	}
	reader := passiveOwnerReader{passiveIntentReader{a.reader}}
	listed, err := validateSettlementEvidenceInventory(ctx, reader, a.pin, reference)
	if err != nil {
		return settlementEvidence{}, err
	}
	evidence, err := loadSettlementEvidence(ctx, reader, a.pin, reference)
	if err != nil {
		return settlementEvidence{}, err
	}
	data, err := a.reader.ReadInfrastructureRecordContext(ctx, settlementName(intent.Sequence))
	if err == nil {
		var record startupSettlement
		if err = task.DecodeStrict(data, &record); err != nil {
			return evidence, err
		}
		if err = evidence.validate(record); err != nil {
			return evidence, err
		}
		return evidence, task.ErrLockBusy
	}
	if !task.RecordAbsent(err) {
		return evidence, err
	}
	if listed {
		return evidence, errors.Join(task.ErrEvidenceFault, err)
	}
	if evidence.assumption == nil {
		expired, expireErr := intent.Expired(time.Now())
		if expireErr != nil {
			return evidence, expireErr
		}
		if expired {
			return evidence, task.ErrLockBusy
		}
	}
	return evidence, nil
}

func unassumedStartupPending(intent StartupIntent, assumption *StartupAssumption) (bool, error) {
	if assumption != nil {
		return false, nil
	}
	expired, err := intent.Expired(time.Now())
	if err != nil {
		return false, err
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return false, err
	}
	return lifetime.CompareBootIDs(intent.Inputs.BootID, boot) == lifetime.BootSame && !expired, nil
}
