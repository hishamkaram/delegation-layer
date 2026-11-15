package infrastructure

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// historicalDaemonWaitPendingError preserves the existing pending cause while
// identifying the one positively observed historical-custodian origin. It lends
// no Wait handle or closure knowledge to the bool settlement port.
type historicalDaemonWaitPendingError struct{ cause error }

func (e *historicalDaemonWaitPendingError) Error() string              { return e.cause.Error() }
func (e *historicalDaemonWaitPendingError) Unwrap() error              { return e.cause }
func (*historicalDaemonWaitPendingError) HistoricalDaemonWaitPending() {}

func newHistoricalDaemonWaitPending() error {
	return &historicalDaemonWaitPendingError{cause: ErrOwnerPending}
}

// ObserveDaemonWait lends only completion knowledge. Exact registered birth/done
// is authoritative for this resident. A missing/pruned entry is never closure:
// only exhaustive canonical history may establish that no live custodian remains.
// The caller's canonical migration owner retains every durable closure witness.
func (w *daemonWaitOwner) ObserveDaemonWait(ctx context.Context, root string, expected lifetime.ProcessRef) (owned bool, closed bool, resultErr error) {
	if w == nil || ctx == nil {
		return false, false, os.ErrInvalid
	}
	if root != w.origin.Root() {
		return false, false, ErrIdentity
	}
	if _, err := lifetime.NewProcessRef(expected.PID(), expected.StartToken()); err != nil {
		return false, false, err
	}
	if err := w.requireServing(ctx); err != nil {
		return false, false, err
	}
	owned, closed, err := w.observeRegisteredWait(ctx, expected)
	if err != nil || owned {
		return owned, closed, err
	}
	if err = w.requireNoHistoricalCustodian(ctx); err != nil {
		return false, false, err
	}
	// Catch a registration completed while history was read. Native Start/birth
	// publication remains serialized by the caller's canonical bootstrap owner.
	owned, closed, err = w.observeRegisteredWait(ctx, expected)
	if err != nil || owned {
		return owned, closed, err
	}
	return false, false, w.requireServing(ctx)
}

func (w *daemonWaitOwner) observeRegisteredWait(ctx context.Context, expected lifetime.ProcessRef) (bool, bool, error) {
	w.mu.Lock()
	if w.registrationClosed {
		w.mu.Unlock()
		return false, false, os.ErrClosed
	}
	candidates := make([]daemonWaitCandidate, 0)
	for done, registration := range w.active {
		if registration.pid == expected.PID() {
			candidates = append(candidates, daemonWaitCandidate{done: done, registration: registration})
		}
	}
	w.mu.Unlock()
	var uncertain error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		if candidate.registration.birth == nil {
			uncertain = errors.Join(uncertain, ErrDaemonWaitCustody)
			continue
		}
		birth, ready, err := candidate.registration.birth()
		if err != nil {
			uncertain = errors.Join(uncertain, ErrDaemonWaitCustody, err)
			continue
		}
		if !ready {
			uncertain = errors.Join(uncertain, ErrOwnerPending)
			continue
		}
		same, err := lifetime.SameProcess(birth, expected)
		if err != nil {
			return false, false, err
		}
		if same {
			select {
			case <-candidate.done:
				return true, true, nil
			default:
				return true, false, nil
			}
		}
	}
	return false, false, errors.Join(uncertain, ctx.Err())
}

func (w *daemonWaitOwner) requireNoHistoricalCustodian(ctx context.Context) error {
	entry := w.resident
	if entry == nil {
		return ErrDaemonWaitCustody
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed || entry.journal == nil {
		return os.ErrClosed
	}
	current, err := ReferenceStartup(entry.operation.Intent())
	if err != nil {
		return err
	}
	return entry.journal.withHistoricalRecords(ctx, func(reader ownerRecordReader, pin taskdir.RootPin) error {
		return observeHistoricalCustodians(ctx, entry.journal, reader, pin, current)
	})
}

func observeHistoricalCustodians(ctx context.Context, journal *intentJournal, reader ownerRecordReader, pin taskdir.RootPin, current StartupReference) error {
	names, err := reader.names(ctx)
	if err != nil {
		return err
	}
	if err = validateWaitHistoryInventory(names, current.Sequence); err != nil {
		return err
	}
	// Validate the captured inventory itself. Re-enumeration could otherwise
	// turn a listed record disappearing between reads into harmless absence.
	for _, name := range names {
		if err = validateNamedStartupEvidence(ctx, reader, pin, name); err != nil {
			return err
		}
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return err
	}
	var pending error
	for sequence := uint64(1); sequence <= current.Sequence; sequence++ {
		if err = observeHistoricalCustodian(ctx, journal, reader, pin, current, sequence, boot); err != nil {
			// Continue only this native-present origin. Every historian keeps its
			// own tag; a later independent fault retains the complete graph.
			if _, knownPending := err.(interface{ HistoricalDaemonWaitPending() }); knownPending {
				pending = errors.Join(pending, err)
				continue
			}
			return errors.Join(pending, err)
		}
	}
	return errors.Join(pending, ctx.Err())
}

func validateWaitHistoryInventory(names []string, latest uint64) error {
	intents := uint64(0)
	for _, name := range names {
		prefix := ""
		for _, candidate := range []string{"intent-", "assumption-", "publication-", "settlement-"} {
			if strings.HasPrefix(name, candidate) {
				prefix = candidate
				break
			}
		}
		if prefix == "" {
			continue
		}
		sequence, err := intentSequence("intent-" + strings.TrimPrefix(name, prefix))
		if err != nil || sequence > latest {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		if prefix == "intent-" {
			intents++
		}
	}
	if intents != latest {
		return task.ErrEvidenceFault
	}
	return nil
}

func observeHistoricalCustodian(ctx context.Context, journal *intentJournal, reader ownerRecordReader, pin taskdir.RootPin, current StartupReference, sequence uint64, boot string) error {
	intent, err := readPinnedIntent(ctx, reader, pin, sequence)
	if err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	reference, err := ReferenceStartup(intent)
	if err != nil {
		return err
	}
	evidence, err := loadSettlementEvidence(ctx, reader, pin, reference)
	if err != nil {
		return err
	}
	if sequence == current.Sequence {
		if reference != current || evidence.assumption == nil || evidence.publication == nil {
			return task.ErrEvidenceFault
		}
		return validateCurrentIntent(intent, pin, boot)
	}
	// Every predecessor was fenced before this resident's election. Missing or
	// malformed linkage cannot be interpreted as a child that never ran.
	var settlement startupSettlement
	if err = journal.control.Read(settlementName(sequence), &settlement); err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	if err = evidence.validate(settlement); err != nil {
		return err
	}
	return observeHistoricalCustodianProcess(ctx, evidence, pin, boot)
}

func observeHistoricalCustodianProcess(ctx context.Context, evidence settlementEvidence, pin taskdir.RootPin, boot string) error {
	switch lifetime.CompareBootIDs(evidence.intent.Inputs.BootID, boot) {
	case lifetime.BootDifferent:
		return nil
	case lifetime.BootUnknown:
		return ErrIdentity
	case lifetime.BootSame:
		if err := validateCurrentIntent(evidence.intent, pin, boot); err != nil {
			return err
		}
	}
	// RunOwnerMode finishes assumption/publication and mints the resident grant
	// before starting any preparation. A fenced unassumed child cannot own Wait.
	if evidence.assumption == nil {
		return nil
	}
	process, err := evidence.assumption.Process.Reference()
	if err != nil {
		return err
	}
	present, err := lifetime.ProcessPresent(process)
	if err != nil {
		return err
	}
	if present {
		// Serving release is deliberately not native exit. There is no durable
		// daemon-to-owner link, so even an older live assumption stays unknown.
		return newHistoricalDaemonWaitPending()
	}
	return ctx.Err()
}
