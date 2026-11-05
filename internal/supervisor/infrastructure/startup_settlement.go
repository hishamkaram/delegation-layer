package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type startupSettlementReason string

const (
	settlementUnassumedExpired startupSettlementReason = "unassumed-expired"
	settlementAssumedAbsent    startupSettlementReason = "assumed-absent"
	settlementPreviousBoot     startupSettlementReason = "previous-boot"
)

// The journal alone writes settlement under the actual instance lease. This
// immutable observation is neither a successor choice nor permission to Start.
// Empty evidence digests mean whole canonical absence at settlement time.
type startupSettlement struct {
	Protocol          int                     `json:"protocol"`
	Startup           StartupReference        `json:"startup"`
	AssumptionSHA256  string                  `json:"assumption_sha256"`
	PublicationSHA256 string                  `json:"publication_sha256"`
	ObservedBootID    string                  `json:"observed_boot_id"`
	SettledAt         string                  `json:"settled_at"`
	Reason            startupSettlementReason `json:"reason"`
}

func (s startupSettlement) Validate(intent StartupIntent) error {
	if s.Protocol != ProtocolVersion {
		return ErrIdentity
	}
	if err := s.Startup.validateIntent(intent); err != nil {
		return err
	}
	observed, err := canonicalStartupTime(s.SettledAt)
	if err != nil {
		return err
	}
	for _, digest := range []string{s.AssumptionSHA256, s.PublicationSHA256} {
		if digest != "" && task.ValidateSHA256(digest) != nil {
			return ErrIdentity
		}
	}
	if s.PublicationSHA256 != "" && s.AssumptionSHA256 == "" {
		return ErrIdentity
	}
	return s.validateReason(intent, observed)
}

func (s startupSettlement) validateReason(intent StartupIntent, observed time.Time) error {
	relation := lifetime.CompareBootIDs(intent.Inputs.BootID, s.ObservedBootID)
	switch s.Reason {
	case settlementPreviousBoot:
		if relation != lifetime.BootDifferent {
			return ErrIdentity
		}
	case settlementAssumedAbsent:
		if relation != lifetime.BootSame || s.AssumptionSHA256 == "" {
			return ErrIdentity
		}
	case settlementUnassumedExpired:
		if relation != lifetime.BootSame || s.AssumptionSHA256 != "" || s.PublicationSHA256 != "" {
			return ErrIdentity
		}
		expired, expireErr := intent.Expired(observed)
		if expireErr != nil || !expired {
			return errors.Join(ErrIdentity, expireErr)
		}
	default:
		return ErrIdentity
	}
	return nil
}

type settlementEvidence struct {
	intent      StartupIntent
	assumption  *StartupAssumption
	publication *OwnerPublication
}

func (e settlementEvidence) bind(record *startupSettlement) error {
	if e.assumption != nil {
		digest, err := e.assumption.Digest(e.intent)
		if err != nil {
			return err
		}
		record.AssumptionSHA256 = digest
	}
	if e.publication != nil {
		if e.assumption == nil {
			return task.ErrEvidenceFault
		}
		digest, err := e.publication.Digest(e.intent, *e.assumption)
		if err != nil {
			return err
		}
		record.PublicationSHA256 = digest
	}
	return nil
}

func (e settlementEvidence) validate(record startupSettlement) error {
	if err := record.Validate(e.intent); err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	expected := startupSettlement{}
	if err := e.bind(&expected); err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	if expected.AssumptionSHA256 != record.AssumptionSHA256 || expected.PublicationSHA256 != record.PublicationSHA256 {
		return errors.Join(task.ErrEvidenceFault, ErrIdentity)
	}
	return nil
}

// A replay is not a new observation time, but a current same-boot live
// incarnation still contradicts settlement. The recorded expiry fact survives
// wall-clock adjustment. Historical reads deliberately use validate, not this.
func (e settlementEvidence) reobserve(record startupSettlement, bootID string) error {
	if err := e.validate(record); err != nil {
		return err
	}
	observedAt, err := canonicalStartupTime(record.SettledAt)
	if err != nil {
		return err
	}
	_, err = observeStartupSettlement(e.intent, e.assumption, bootID, observedAt, lifetime.ProcessPresent)
	return err
}

// settleStartup derives every observation within one checked lease transaction.
// An existing equal observation returns created=false and grants no fresh work.
func (j *intentJournal) settleStartup(ctx context.Context, lease *Lease, reference StartupReference) (record startupSettlement, created bool, resultErr error) {
	resultErr = j.withLeaseTransaction(ctx, lease, func(tx *taskdir.ControlTransaction) error {
		reader := transactionStartupReader{transactionIntentReader{j.control, tx}}
		evidence, bootID, listed, err := j.currentSettlementEvidence(ctx, reader, reference)
		if err != nil {
			return err
		}
		if err = tx.Read(settlementName(reference.Sequence), &record); err == nil {
			if err = evidence.reobserve(record, bootID); err != nil {
				return err
			}
			return tx.Verify(settlementName(reference.Sequence), record)
		}
		if !task.RecordAbsent(err) {
			return err
		}
		if listed {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		record, err = evidence.observe(reference, bootID, time.Now().UTC())
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = j.checkRoot(ctx); err != nil {
			return err
		}
		created, err = tx.Put(settlementName(reference.Sequence), record)
		return err
	})
	return record, created, resultErr
}

// Called only within the checked lease transaction; the result remains data.
func (e settlementEvidence) observe(reference StartupReference, bootID string, observedAt time.Time) (startupSettlement, error) {
	reason, err := observeStartupSettlement(e.intent, e.assumption, bootID, observedAt, lifetime.ProcessPresent)
	if err != nil {
		return startupSettlement{}, err
	}
	record := startupSettlement{Protocol: ProtocolVersion, Startup: reference, ObservedBootID: bootID, SettledAt: observedAt.Format(time.RFC3339Nano), Reason: reason}
	if err = e.bind(&record); err != nil {
		return startupSettlement{}, err
	}
	if err = record.Validate(e.intent); err != nil {
		return startupSettlement{}, err
	}
	return record, nil
}

func (j *intentJournal) currentSettlementEvidence(ctx context.Context, reader ownerRecordReader, reference StartupReference) (settlementEvidence, string, bool, error) {
	if err := reference.Validate(); err != nil {
		return settlementEvidence{}, "", false, err
	}
	latest, found, err := latestPinnedIntent(ctx, reader, j.pin)
	if err != nil {
		return settlementEvidence{}, "", false, err
	}
	if !found {
		return settlementEvidence{}, "", false, os.ErrNotExist
	}
	if err = reference.validateIntent(latest); err != nil {
		return settlementEvidence{}, "", false, err
	}
	listed, err := validateSettlementEvidenceInventory(ctx, reader, j.pin, reference)
	if err != nil {
		return settlementEvidence{}, "", false, err
	}
	evidence, err := loadSettlementEvidence(ctx, reader, j.pin, reference)
	if err != nil {
		return settlementEvidence{}, "", false, err
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return settlementEvidence{}, "", false, err
	}
	if lifetime.CompareBootIDs(j.bootID, bootID) != lifetime.BootSame {
		return settlementEvidence{}, "", false, ErrIdentity
	}
	if err = validateSettlementBoot(evidence.intent, j.pin, bootID); err != nil {
		return settlementEvidence{}, "", false, err
	}
	return evidence, bootID, listed, nil
}

func validateSettlementBoot(intent StartupIntent, pin taskdir.RootPin, bootID string) error {
	switch lifetime.CompareBootIDs(intent.Inputs.BootID, bootID) {
	case lifetime.BootSame:
		return validateCurrentIntent(intent, pin, bootID)
	case lifetime.BootDifferent:
		return validateHistoricalIntent(intent, pin)
	case lifetime.BootUnknown:
		return ErrIdentity
	default:
		return ErrIdentity
	}
}

// The observer is supplied only by the journal above. Keeping this narrow
// decision callable proves that previous-boot history never probes an old PID.
func observeStartupSettlement(intent StartupIntent, assumption *StartupAssumption, bootID string, now time.Time, present func(lifetime.ProcessRef) (bool, error)) (startupSettlementReason, error) {
	switch lifetime.CompareBootIDs(intent.Inputs.BootID, bootID) {
	case lifetime.BootDifferent:
		return settlementPreviousBoot, nil
	case lifetime.BootUnknown:
		return "", ErrIdentity
	case lifetime.BootSame:
		// Only this boot may interpret the saved native process incarnation.
	default:
		return "", ErrIdentity
	}
	if assumption == nil {
		expired, err := intent.Expired(now)
		if err != nil {
			return "", err
		}
		if !expired {
			return "", fmt.Errorf("%w: original unassumed startup deadline has not expired", ErrIdentity)
		}
		return settlementUnassumedExpired, nil
	}
	reference, err := assumption.Process.Reference()
	if err != nil {
		return "", err
	}
	alive, err := present(reference)
	if err != nil {
		return "", err
	}
	if alive {
		return "", fmt.Errorf("%w: assumed startup incarnation remains present", ErrIdentity)
	}
	return settlementAssumedAbsent, nil
}

func loadSettlementEvidence(ctx context.Context, reader ownerRecordReader, pin taskdir.RootPin, reference StartupReference) (settlementEvidence, error) {
	intent, err := readReferencedIntent(ctx, reader, pin, reference)
	if err != nil {
		return settlementEvidence{}, err
	}
	evidence := settlementEvidence{intent: intent}
	assumption, err := readAssumptionRecord(ctx, reader, pin, reference)
	if err == nil {
		evidence.assumption = &assumption
	} else if !task.RecordAbsent(err) {
		return settlementEvidence{}, err
	}
	publication, err := reader.publication(ctx, publicationName(reference.Sequence))
	if task.RecordAbsent(err) {
		return evidence, nil
	}
	if err != nil {
		return settlementEvidence{}, err
	}
	if evidence.assumption == nil {
		return settlementEvidence{}, fmt.Errorf("%w: owner publication has no assumption", task.ErrEvidenceFault)
	}
	if _, err = publication.Digest(intent, *evidence.assumption); err != nil {
		return settlementEvidence{}, errors.Join(task.ErrEvidenceFault, err)
	}
	evidence.publication = &publication
	return evidence, nil
}

func validateSettlementEvidenceInventory(ctx context.Context, reader ownerRecordReader, pin taskdir.RootPin, reference StartupReference) (bool, error) {
	names, err := reader.names(ctx)
	if err != nil {
		return false, err
	}
	listed := false
	for _, name := range names {
		if strings.HasPrefix(name, "settlement-") {
			sequence, parseErr := intentSequence("intent-" + strings.TrimPrefix(name, "settlement-"))
			if parseErr != nil {
				return false, parseErr
			}
			listed = listed || sequence == reference.Sequence
			continue
		}
		if err = validateNamedStartupEvidence(ctx, reader, pin, name); err != nil {
			return false, err
		}
	}
	return listed, nil
}

func validateNamedStartupEvidence(ctx context.Context, reader ownerRecordReader, pin taskdir.RootPin, name string) error {
	prefix := startupEvidencePrefix(name)
	if prefix == "" {
		return nil
	}
	sequence, parseErr := intentSequence("intent-" + strings.TrimPrefix(name, prefix))
	if parseErr != nil {
		return parseErr
	}
	intent, readErr := readPinnedIntent(ctx, reader, pin, sequence)
	if readErr != nil {
		return errors.Join(task.ErrEvidenceFault, readErr)
	}
	recordReference, referenceErr := ReferenceStartup(intent)
	if referenceErr != nil {
		return referenceErr
	}
	evidence, evidenceErr := loadSettlementEvidence(ctx, reader, pin, recordReference)
	if evidenceErr != nil {
		return errors.Join(task.ErrEvidenceFault, evidenceErr)
	}
	if (prefix == "assumption-" && evidence.assumption == nil) || (prefix == "publication-" && evidence.publication == nil) {
		return errors.Join(task.ErrEvidenceFault, os.ErrNotExist)
	}
	return nil
}

func startupEvidencePrefix(name string) string {
	for _, prefix := range []string{"assumption-", "publication-"} {
		if strings.HasPrefix(name, prefix) {
			return prefix
		}
	}
	return ""
}

func (j *intentJournal) readSettlement(ctx context.Context, reference StartupReference) (record startupSettlement, resultErr error) {
	resultErr = j.withHistoricalRecords(ctx, func(reader ownerRecordReader, pin taskdir.RootPin) error {
		evidence, err := loadSettlementEvidence(ctx, reader, pin, reference)
		if err != nil {
			return err
		}
		if err = j.control.Read(settlementName(reference.Sequence), &record); err != nil {
			return err
		}
		return evidence.validate(record)
	})
	return record, resultErr
}

func settlementName(sequence uint64) string { return fmt.Sprintf("settlement-%020d.json", sequence) }

// Enumeration is a read of the already-owned ControlDir; all record reads use
// the active transaction, without recursively entering ControlDir.WithLock.
type transactionIntentReader struct {
	control *taskdir.ControlDir
	tx      *taskdir.ControlTransaction
}

func (r transactionIntentReader) names(ctx context.Context) ([]string, error) {
	return r.control.RecordNamesContext(ctx)
}

func (r transactionIntentReader) read(ctx context.Context, name string) (StartupIntent, error) {
	if err := ctx.Err(); err != nil {
		return StartupIntent{}, err
	}
	var record StartupIntent
	if err := r.tx.Read(name, &record); err != nil {
		return StartupIntent{}, err
	}
	return record, ctx.Err()
}

type transactionStartupReader struct{ transactionIntentReader }

func (r transactionStartupReader) assumption(ctx context.Context, name string) (StartupAssumption, error) {
	if err := ctx.Err(); err != nil {
		return StartupAssumption{}, err
	}
	var record StartupAssumption
	if err := r.tx.Read(name, &record); err != nil {
		return StartupAssumption{}, err
	}
	return record, ctx.Err()
}

func (r transactionStartupReader) publication(ctx context.Context, name string) (OwnerPublication, error) {
	if err := ctx.Err(); err != nil {
		return OwnerPublication{}, err
	}
	var record OwnerPublication
	if err := r.tx.Read(name, &record); err != nil {
		return OwnerPublication{}, err
	}
	return record, ctx.Err()
}
