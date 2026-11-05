package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The infrastructure journal is the sole owner of immutable lifecycle bodies
// and fixed, create-once phase records. Coordinators supply native observations
// while retaining their real leases; these readers and records grant none.
// No callback here may enter Pueue or acquire bootstrap: bootstrap precedes the
// journal borrow, instance descriptor guard and short ControlDir transaction.
type epochLifecycleSnapshot struct {
	intent  epochLifecycleIntent
	records []epochLifecycleProgress
	phase   epochLifecyclePhase
}

type lifecycleRecordReader interface {
	ownerRecordReader
	lifecycle(context.Context, string) (epochLifecycleIntent, error)
	progress(context.Context, string) (epochLifecycleProgress, error)
	settlement(context.Context, string) (startupSettlement, error)
}

type controlLifecycleReader struct{ controlOwnerReader }

func (r controlLifecycleReader) lifecycle(ctx context.Context, name string) (record epochLifecycleIntent, err error) {
	if err = ctx.Err(); err == nil {
		err = r.control.Read(name, &record)
	}
	return record, err
}

func (r controlLifecycleReader) progress(ctx context.Context, name string) (record epochLifecycleProgress, err error) {
	if err = ctx.Err(); err == nil {
		err = r.control.Read(name, &record)
	}
	return record, err
}

type transactionLifecycleReader struct{ transactionStartupReader }

func (r transactionLifecycleReader) lifecycle(ctx context.Context, name string) (record epochLifecycleIntent, err error) {
	if err = ctx.Err(); err == nil {
		err = r.tx.Read(name, &record)
	}
	return record, err
}

func (r transactionLifecycleReader) progress(ctx context.Context, name string) (record epochLifecycleProgress, err error) {
	if err = ctx.Err(); err == nil {
		err = r.tx.Read(name, &record)
	}
	return record, err
}

type passiveLifecycleReader struct{ passiveOwnerReader }

func (r passiveLifecycleReader) names(ctx context.Context) ([]string, error) {
	names, err := r.store.InfrastructureRecordNamesContext(ctx)
	if task.RecordAbsent(err) {
		return []string{}, nil
	}
	return names, err
}

func (r passiveLifecycleReader) lifecycle(ctx context.Context, name string) (record epochLifecycleIntent, err error) {
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err == nil {
		err = task.DecodeStrict(data, &record)
	}
	return record, err
}

func (r passiveLifecycleReader) progress(ctx context.Context, name string) (record epochLifecycleProgress, err error) {
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err == nil {
		err = task.DecodeStrict(data, &record)
	}
	return record, err
}

func lifecycleName(id string) string { return "lifecycle-" + id + ".json" }
func lifecycleProgressName(id string, phase epochLifecyclePhase) string {
	return "lifecycle-" + id + "-" + string(phase) + ".json"
}

func parseLifecycleName(name string) (id string, phase epochLifecyclePhase, err error) {
	suffix := strings.TrimSuffix(strings.TrimPrefix(name, "lifecycle-"), ".json")
	if !strings.HasPrefix(name, "lifecycle-") || !strings.HasSuffix(name, ".json") || len(suffix) < 32 {
		return "", "", task.ErrEvidenceFault
	}
	id = suffix[:32]
	if err = task.ValidateTaskID(id); err != nil {
		return "", "", errors.Join(task.ErrEvidenceFault, err)
	}
	if len(suffix) == 32 {
		return id, "", nil
	}
	if suffix[32] != '-' {
		return "", "", task.ErrEvidenceFault
	}
	phase = epochLifecyclePhase(suffix[33:])
	switch phase {
	case epochLifecycleClosing, epochLifecycleStopping, epochLifecycleExiting, epochLifecycleComplete, epochLifecycleAborted, epochLifecycleAbandoned:
		return id, phase, nil
	case epochLifecycleAccepted:
		return "", "", task.ErrEvidenceFault
	default:
		return "", "", task.ErrEvidenceFault
	}
}

type lifecycleHistoryRecords struct {
	bodies   map[string]epochLifecycleIntent
	progress map[string]map[epochLifecyclePhase]epochLifecycleProgress
}

func readLifecycleHistory(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin) ([]epochLifecycleSnapshot, error) {
	names, err := reader.names(ctx)
	if err != nil {
		return nil, err
	}
	records := lifecycleHistoryRecords{map[string]epochLifecycleIntent{}, map[string]map[epochLifecyclePhase]epochLifecycleProgress{}}
	for _, name := range names {
		if !strings.HasPrefix(name, "lifecycle-") {
			continue
		}
		if err = records.read(ctx, reader, pin, name); err != nil {
			return nil, err
		}
	}
	history, err := assembleLifecycleHistory(records.bodies, records.progress)
	if err != nil {
		return nil, err
	}
	if err = validateLifecycleHistoryLinks(ctx, reader, pin, history); err != nil {
		return nil, errors.Join(task.ErrEvidenceFault, err)
	}
	return history, nil
}

func (r *lifecycleHistoryRecords) read(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, name string) error {
	id, phase, err := parseLifecycleName(name)
	if err != nil {
		return err
	}
	if phase == "" {
		record, readErr := reader.lifecycle(ctx, name)
		if readErr != nil {
			return errors.Join(task.ErrEvidenceFault, readErr)
		}
		if record.ID != id {
			return task.ErrEvidenceFault
		}
		if readErr = validateHistoricalLifecycle(record, pin); readErr != nil {
			return errors.Join(task.ErrEvidenceFault, readErr)
		}
		r.bodies[id] = record
		return nil
	}
	record, err := reader.progress(ctx, name)
	if err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	if record.Phase != phase {
		return task.ErrEvidenceFault
	}
	if r.progress[id] == nil {
		r.progress[id] = map[epochLifecyclePhase]epochLifecycleProgress{}
	}
	r.progress[id][phase] = record
	return nil
}

func assembleLifecycleHistory(bodies map[string]epochLifecycleIntent, progress map[string]map[epochLifecyclePhase]epochLifecycleProgress) ([]epochLifecycleSnapshot, error) {
	for id := range progress {
		if _, found := bodies[id]; !found {
			return nil, task.ErrEvidenceFault
		}
	}
	history := make([]epochLifecycleSnapshot, 0, len(bodies))
	for id, body := range bodies {
		records := []epochLifecycleProgress{}
		for _, phase := range []epochLifecyclePhase{epochLifecycleClosing, epochLifecycleStopping, epochLifecycleExiting, epochLifecycleComplete, epochLifecycleAborted, epochLifecycleAbandoned} {
			if record, found := progress[id][phase]; found {
				records = append(records, record)
			}
		}
		phase, err := deriveEpochLifecyclePhase(body, records)
		if err != nil {
			return nil, errors.Join(task.ErrEvidenceFault, err)
		}
		history = append(history, epochLifecycleSnapshot{body, records, phase})
	}
	sort.Slice(history, func(i, j int) bool { return history[i].intent.Sequence < history[j].intent.Sequence })
	for index, snapshot := range history {
		if snapshot.intent.Sequence != uint64(index)+1 {
			return nil, task.ErrEvidenceFault
		}
		if index+1 < len(history) && !lifecycleTerminal(snapshot.phase) {
			return nil, task.ErrEvidenceFault
		}
	}
	return history, nil
}

func latestLifecycle(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin) (epochLifecycleSnapshot, bool, error) {
	history, err := readLifecycleHistory(ctx, reader, pin)
	if err != nil || len(history) == 0 {
		return epochLifecycleSnapshot{}, false, err
	}
	return history[len(history)-1], true, nil
}

func readReferencedLifecycle(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, reference epochLifecycleReference) (epochLifecycleSnapshot, error) {
	if err := reference.Validate(); err != nil {
		return epochLifecycleSnapshot{}, err
	}
	history, err := readLifecycleHistory(ctx, reader, pin)
	if err != nil {
		return epochLifecycleSnapshot{}, err
	}
	for _, snapshot := range history {
		if snapshot.intent.ID == reference.ID {
			return snapshot, reference.validateIntent(snapshot.intent)
		}
	}
	return epochLifecycleSnapshot{}, fmt.Errorf("%w: referenced lifecycle body is missing", task.ErrEvidenceFault)
}

func lifecycleTerminal(phase epochLifecyclePhase) bool {
	return phase == epochLifecycleComplete || phase == epochLifecycleAborted || phase == epochLifecycleAbandoned
}

func validateHistoricalLifecycle(body epochLifecycleIntent, pin taskdir.RootPin) error {
	if err := body.Validate(); err != nil {
		return err
	}
	if body.Root != pin.Root() || body.RootID != pin.RootID() {
		return ErrIdentity
	}
	return nil
}

// acceptLifecycle is a create-once record mutation, never permission to act.
// Resident callers retain their own instance EX. Recovered outside callers
// retain a newly acquired EX and must have settled every prior startup.
func (j *intentJournal) acceptLifecycle(ctx context.Context, lease *Lease, body epochLifecycleIntent) (created bool, resultErr error) {
	resultErr = j.withLeaseTransaction(ctx, lease, func(tx *taskdir.ControlTransaction) error {
		boot, err := j.currentLifecycleBoot()
		if err != nil {
			return err
		}
		if err = validateCurrentLifecycle(body, j.pin, boot); err != nil {
			return err
		}
		reader := transactionLifecycleReader{transactionStartupReader{transactionIntentReader{j.control, tx}}}
		existing, err := j.prepareLifecycleAcceptance(ctx, tx, reader, boot, body)
		if err != nil {
			return err
		}
		if existing {
			return tx.Verify(lifecycleName(body.ID), body)
		}
		created, err = tx.Put(lifecycleName(body.ID), body)
		return err
	})
	return created, resultErr
}

func (j *intentJournal) prepareLifecycleAcceptance(ctx context.Context, tx *taskdir.ControlTransaction, reader transactionLifecycleReader, boot string, body epochLifecycleIntent) (bool, error) {
	history, err := readLifecycleHistory(ctx, reader, j.pin)
	if err != nil {
		return false, err
	}
	for _, saved := range history {
		if saved.intent.ID == body.ID {
			return true, nil
		}
	}
	deadline, err := canonicalStartupTime(body.Deadline)
	if err != nil {
		return false, err
	}
	if !time.Now().Before(deadline) {
		return false, context.DeadlineExceeded
	}
	if err = validateLifecycleAppend(history, body); err != nil {
		return false, err
	}
	if err = validateLifecycleOwner(ctx, reader, j.pin, boot, history, body); err != nil {
		return false, err
	}
	if body.Kind == epochLifecycleOwnerUpgrade {
		selected, readErr := readContextRecords(ctx, transactionContextReader{tx}, body.SuccessorContext, j.pin)
		if readErr != nil {
			return false, readErr
		}
		if selected.OS != runtime.GOOS {
			return false, ErrIdentity
		}
	}
	return false, nil
}

func validateLifecycleAppend(history []epochLifecycleSnapshot, body epochLifecycleIntent) error {
	if uint64(len(history)) == ^uint64(0) || body.Sequence != uint64(len(history))+1 {
		return ErrIdentity
	}
	if len(history) != 0 && !lifecycleTerminal(history[len(history)-1].phase) {
		return task.ErrLockBusy
	}
	return nil
}

func validateCurrentLifecycle(body epochLifecycleIntent, pin taskdir.RootPin, boot string) error {
	if err := validateHistoricalLifecycle(body, pin); err != nil {
		return err
	}
	if lifetime.CompareBootIDs(body.BootID, boot) != lifetime.BootSame || body.RootDevice != pin.Device() || body.RootInode != pin.Inode() {
		return ErrIdentity
	}
	return nil
}

func (j *intentJournal) currentLifecycleBoot() (string, error) {
	boot, err := lifetime.BootID()
	if err != nil {
		return "", err
	}
	if lifetime.CompareBootIDs(boot, j.bootID) != lifetime.BootSame {
		return "", ErrIdentity
	}
	return boot, nil
}

func validateLifecycleOwner(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, boot string, history []epochLifecycleSnapshot, body epochLifecycleIntent) error {
	latest, found, err := latestPinnedIntent(ctx, reader, pin)
	if err != nil {
		return err
	}
	if !found {
		if body.Owner != (StartupReference{}) {
			return ErrIdentity
		}
		return nil
	}
	reference, err := ReferenceStartup(latest)
	if err != nil {
		return err
	}
	if len(history) != 0 && history[len(history)-1].phase == epochLifecycleAbandoned {
		// Carry the exact logical predecessor, including an original zero. A
		// failed tentative successor never becomes a new target selection.
		if body.Owner != history[len(history)-1].intent.Owner {
			return ErrIdentity
		}
		return requireLifecycleSettlements(ctx, reader, pin, boot, StartupReference{})
	}
	if body.Owner != reference {
		return ErrIdentity
	}
	if err = validateSettlementBoot(latest, pin, boot); err != nil {
		return err
	}
	resident, err := lifecycleOwnerIsSelf(ctx, reader, pin, boot, latest, reference)
	if err != nil {
		return err
	}
	if !resident {
		reference = StartupReference{}
	}
	return requireLifecycleSettlements(ctx, reader, pin, boot, reference)
}

func lifecycleOwnerIsSelf(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, boot string, intent StartupIntent, reference StartupReference) (bool, error) {
	if lifetime.CompareBootIDs(intent.Inputs.BootID, boot) != lifetime.BootSame {
		return false, nil
	}
	assumption, err := readAssumptionRecord(ctx, reader, pin, reference)
	if task.RecordAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if assumption.Process.PID != os.Getpid() || assumption.UID != os.Geteuid() {
		return false, nil
	}
	process, err := assumption.Process.Reference()
	if err != nil {
		return false, err
	}
	present, err := lifetime.ProcessPresent(process)
	if err != nil || !present {
		return false, err
	}
	publication, err := readPublicationRecord(ctx, reader, pin, reference)
	if err != nil {
		return false, err
	}
	if err = validateLifecycleRuntime(ctx, reader, pin, intent, publication.RuntimeSHA256); err != nil {
		return false, err
	}
	return true, nil
}

func requireLifecycleSettlements(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, boot string, except StartupReference) error {
	names, err := reader.names(ctx)
	if err != nil {
		return err
	}
	sequences := []uint64{}
	for _, name := range names {
		if !strings.HasPrefix(name, "intent-") {
			continue
		}
		sequence, parseErr := intentSequence(name)
		if parseErr != nil {
			return parseErr
		}
		sequences = append(sequences, sequence)
	}
	slices.Sort(sequences)
	for index, sequence := range sequences {
		if sequence != uint64(index)+1 {
			return task.ErrEvidenceFault
		}
		intent, readErr := readPinnedIntent(ctx, reader, pin, sequence)
		if readErr != nil {
			return errors.Join(task.ErrEvidenceFault, readErr)
		}
		reference, referenceErr := ReferenceStartup(intent)
		if referenceErr != nil {
			return referenceErr
		}
		if reference == except {
			continue
		}
		if err = validateSettledLifecycleStartup(ctx, reader, pin, boot, reference); err != nil {
			return err
		}
	}
	return nil
}

func validateSettledLifecycleStartup(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, boot string, reference StartupReference) error {
	evidence, err := loadSettlementEvidence(ctx, reader, pin, reference)
	if err != nil {
		return err
	}
	if err = validateLifecycleStartupHistory(ctx, reader, pin, evidence.intent); err != nil {
		return err
	}
	if evidence.publication != nil {
		if err = validateLifecycleRuntime(ctx, reader, pin, evidence.intent, evidence.publication.RuntimeSHA256); err != nil {
			return err
		}
	}
	if err = validateSettlementBoot(evidence.intent, pin, boot); err != nil {
		return err
	}
	record, err := readLifecycleSettlement(ctx, reader, reference.Sequence)
	if err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	return evidence.reobserve(record, boot)
}

func readLifecycleSettlement(ctx context.Context, reader lifecycleRecordReader, sequence uint64) (startupSettlement, error) {
	return reader.settlement(ctx, settlementName(sequence))
}

func (r controlLifecycleReader) settlement(ctx context.Context, name string) (record startupSettlement, err error) {
	if err = ctx.Err(); err == nil {
		err = r.control.Read(name, &record)
	}
	return record, err
}

func (r transactionLifecycleReader) settlement(ctx context.Context, name string) (record startupSettlement, err error) {
	if err = ctx.Err(); err == nil {
		err = r.tx.Read(name, &record)
	}
	return record, err
}

func (r passiveLifecycleReader) settlement(ctx context.Context, name string) (record startupSettlement, err error) {
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err == nil {
		err = task.DecodeStrict(data, &record)
	}
	return record, err
}

// appendLifecycleProgress validates storage precedence and exact current-body
// references only. The private phase coordinator must first derive every
// required native/control/fence observation while keeping its real leases.
// No callback here invokes Pueue, performs Start, drains or waits externally.
func (j *intentJournal) appendLifecycleProgress(ctx context.Context, lease *Lease, reference epochLifecycleReference, progress epochLifecycleProgress) (created bool, resultErr error) {
	resultErr = j.withLeaseTransaction(ctx, lease, func(tx *taskdir.ControlTransaction) error {
		boot, err := j.currentLifecycleBoot()
		if err != nil {
			return err
		}
		reader := transactionLifecycleReader{transactionStartupReader{transactionIntentReader{j.control, tx}}}
		latest, found, err := latestLifecycle(ctx, reader, j.pin)
		if err != nil {
			return err
		}
		if !found {
			return task.ErrEvidenceFault
		}
		if err = reference.validateIntent(latest.intent); err != nil {
			return err
		}
		if progress.OperationRef != reference {
			return ErrIdentity
		}
		if err = progress.Validate(latest.intent); err != nil {
			return err
		}
		var saved epochLifecycleProgress
		name := lifecycleProgressName(reference.ID, progress.Phase)
		if err = tx.Read(name, &saved); err == nil {
			return tx.Verify(name, progress)
		}
		if !task.RecordAbsent(err) {
			return err
		}
		if !epochLifecycleTransitionAllowed(latest.phase, progress.Phase) {
			return ErrIdentity
		}
		if err = validateLifecycleProgressWriter(ctx, reader, j.pin, boot, latest.intent, progress); err != nil {
			return err
		}
		created, err = tx.Put(name, progress)
		return err
	})
	return created, resultErr
}

func validateLifecycleProgressWriter(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, boot string, body epochLifecycleIntent, progress epochLifecycleProgress) error {
	if progress.Phase == epochLifecycleStopping && lifetime.CompareBootIDs(progress.StopTarget.BootID, boot) != lifetime.BootSame {
		return ErrIdentity
	}
	if progress.Phase == epochLifecycleAbandoned || (progress.Phase == epochLifecycleComplete && body.Kind == epochLifecycleStop) {
		return requireLifecycleSettlements(ctx, reader, pin, boot, StartupReference{})
	}
	if progress.Phase != epochLifecycleComplete {
		return nil
	}
	latest, found, err := latestPinnedIntent(ctx, reader, pin)
	if err != nil {
		return err
	}
	if !found || latest.Inputs.Kind != StartupUpgrade {
		return ErrIdentity
	}
	if err = progress.Successor.validateIntent(latest); err != nil {
		return err
	}
	if err = (epochLifecycleReference{ID: latest.Inputs.UpgradeID, SHA256: latest.Inputs.UpgradeSHA256}).validateIntent(body); err != nil {
		return err
	}
	if err = validateCurrentIntent(latest, pin, boot); err != nil {
		return err
	}
	resident, err := lifecycleOwnerIsSelf(ctx, reader, pin, boot, latest, progress.Successor)
	if err != nil {
		return err
	}
	if !resident {
		return ErrIdentity
	}
	return requireLifecycleSettlements(ctx, reader, pin, boot, progress.Successor)
}

func (j *intentJournal) latestLifecycle(ctx context.Context) (snapshot epochLifecycleSnapshot, found bool, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		snapshot, found, err = latestLifecycle(ctx, controlLifecycleReader{controlOwnerReader{controlIntentReader{j.control}}}, j.pin)
		return err
	})
	return snapshot, found, resultErr
}

func (j *intentJournal) readLifecycle(ctx context.Context, reference epochLifecycleReference) (snapshot epochLifecycleSnapshot, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		snapshot, err = readReferencedLifecycle(ctx, controlLifecycleReader{controlOwnerReader{controlIntentReader{j.control}}}, j.pin, reference)
		return err
	})
	return snapshot, resultErr
}

func readLatestLifecycle(ctx context.Context, store *taskdir.ReadOnlyStore) (epochLifecycleSnapshot, bool, error) {
	if ctx == nil || store == nil {
		return epochLifecycleSnapshot{}, false, os.ErrInvalid
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return epochLifecycleSnapshot{}, false, err
	}
	snapshot, found, err := latestLifecycle(ctx, passiveLifecycleReader{passiveOwnerReader{passiveIntentReader{store}}}, pin)
	if err != nil {
		return epochLifecycleSnapshot{}, false, err
	}
	current, err := store.PinRootContext(ctx)
	if err == nil && !pin.Equal(current) {
		err = ErrIdentity
	}
	return snapshot, found, err
}

// Cross-family links are strict data validation, never current native probes.
func validateLifecycleHistoryLinks(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, history []epochLifecycleSnapshot) error {
	for index, snapshot := range history {
		if index > 0 && history[index-1].phase == epochLifecycleAbandoned && snapshot.intent.Owner != history[index-1].intent.Owner {
			return ErrIdentity
		}
		if err := validateLifecycleSnapshotLinks(ctx, reader, pin, snapshot); err != nil {
			return err
		}
	}
	return nil
}

func validateLifecycleSnapshotLinks(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, snapshot epochLifecycleSnapshot) error {
	if snapshot.intent.Owner != (StartupReference{}) {
		if _, err := readReferencedIntent(ctx, reader, pin, snapshot.intent.Owner); err != nil {
			return err
		}
	}
	if snapshot.phase != epochLifecycleComplete || snapshot.intent.Kind != epochLifecycleOwnerUpgrade {
		return nil
	}
	evidence, err := loadSettlementEvidence(ctx, reader, pin, lifecycleSuccessor(snapshot))
	if err != nil {
		return err
	}
	if err = validateUpgradeLifecycleBody(evidence.intent, snapshot.intent); err != nil {
		return err
	}
	if evidence.assumption == nil || evidence.publication == nil {
		return task.ErrEvidenceFault
	}
	if evidence.publication.RuntimeSHA256 != snapshot.intent.SuccessorArtifacts.RunnerSHA256 {
		return ErrIdentity
	}
	return nil
}
