package task

import (
	"errors"
	"fmt"
	"time"
)

// LifetimeWorkerWaitAbsent records a durable worker-absence witness. It is a
// completion disposition, not a successful native Wait.
const LifetimeWorkerWaitAbsent LifetimeWorkerWaitResult = "absent"

// LifetimeAbsenceProof identifies the precise witness used for a worker
// absence disposition.
type LifetimeAbsenceProof string

const (
	LifetimeAbsenceExactWorker          LifetimeAbsenceProof = "exact-worker"
	LifetimeAbsencePreviousBoot         LifetimeAbsenceProof = "previous-boot"
	LifetimeAbsenceUnregisteredDisposed LifetimeAbsenceProof = "unregistered-disposed"
)

// LifetimeEvidence is the typed set of lifetime records available to a pure
// completion validator or closure resolver.
type LifetimeEvidence struct {
	Intent       *LifetimeIntentRecord
	Registration *LifetimeRegistrationRecord
	Worker       *LifetimeWorkerRecord
	WorkerWait   *LifetimeWorkerWaitRecord
	Launches     []LifetimeLaunchRecord
	Termination  *LifetimeTerminationRecord
}

// LifetimeClosureKind is the pure closure classification of lifetime facts.
type LifetimeClosureKind string

const (
	LifetimeClosureOpen                  LifetimeClosureKind = "open"
	LifetimeClosureRegisteredNative      LifetimeClosureKind = "registered-native"
	LifetimeClosureRegisteredRecovered   LifetimeClosureKind = "registered-recovered"
	LifetimeClosureLegacyPreviousBoot    LifetimeClosureKind = "legacy-previous-boot"
	LifetimeClosureUnregisteredRecovered LifetimeClosureKind = "unregistered-recovered"
)

// LifetimeClosure is the pure result of resolving typed lifetime evidence.
type LifetimeClosure struct {
	Kind LifetimeClosureKind
}

// Closed reports whether the closure classification is terminal.
func (c LifetimeClosure) Closed() bool {
	switch c.Kind {
	case LifetimeClosureOpen:
		return false
	case LifetimeClosureRegisteredNative,
		LifetimeClosureRegisteredRecovered,
		LifetimeClosureLegacyPreviousBoot,
		LifetimeClosureUnregisteredRecovered:
		return true
	}
	return false
}

// ValidateLifetimeCompletionBinding validates one worker completion
// disposition against the typed records that it claims to bind.
func ValidateLifetimeCompletionBinding(record *LifetimeWorkerWaitRecord, evidence LifetimeEvidence) error {
	if record == nil {
		return errors.New("nil lifetime worker completion")
	}
	if evidence.WorkerWait != nil {
		recordDigest, err := lifetimeCompletionDigest(record)
		if err != nil {
			return err
		}
		evidenceDigest, err := lifetimeCompletionDigest(evidence.WorkerWait)
		if err != nil {
			return err
		}
		if recordDigest != evidenceDigest {
			return ErrIdentityMismatch
		}
	}
	evidence.WorkerWait = record
	if err := validateLifetimeEvidenceFacts(evidence); err != nil {
		return err
	}

	if record.Result == LifetimeWorkerWaitAbsent {
		return validateLifetimeAbsenceBinding(record, evidence, evidence.Registration, evidence.Intent, evidence.Worker)
	}
	return validateLifetimeNativeCompletionBinding(record, evidence)
}

func validateLifetimeNativeCompletionBinding(record *LifetimeWorkerWaitRecord, evidence LifetimeEvidence) error {
	registration, intent, worker := evidence.Registration, evidence.Intent, evidence.Worker
	if registration == nil {
		return ErrIdentityMismatch
	}
	registrationDigest, err := lifetimeCompletionDigest(registration)
	if err != nil {
		return err
	}
	if !LifetimeWorkerWaitMatchesRegistration(record, registration, registrationDigest) {
		return ErrIdentityMismatch
	}
	if intent != nil && !LifetimeIntentMatchesRegistration(intent, registration) {
		return ErrIdentityMismatch
	}
	if worker == nil {
		if LifetimeWorkerWaitProvesNotStarted(record) {
			if len(evidence.Launches) != 0 {
				return ErrIdentityMismatch
			}
			return nil
		}
		if LifetimeWorkerWaitIsQualified(record) || registration.Assurance != LifetimeAssuranceExact {
			return ErrIdentityMismatch
		}
		return nil
	}
	workerDigest, err := lifetimeCompletionDigest(worker)
	if err != nil {
		return err
	}
	if worker.RegistrationSHA256 != registrationDigest || worker.BootID != registration.BootID ||
		!LifetimeWorkerWaitMatchesWorker(record, worker, workerDigest) {
		return ErrIdentityMismatch
	}
	return nil
}

// ResolveLifetimeClosure returns the single closure decision for complete or
// recoverable lifetime evidence. It performs no I/O and makes no native
// process claim.
func ResolveLifetimeClosure(evidence LifetimeEvidence) (LifetimeClosure, error) {
	if err := validateLifetimeEvidenceFacts(evidence); err != nil {
		return LifetimeClosure{}, lifetimeEvidenceFault(err)
	}
	if evidence.WorkerWait != nil {
		if err := ValidateLifetimeCompletionBinding(evidence.WorkerWait, evidence); err != nil {
			return LifetimeClosure{}, lifetimeEvidenceFault(err)
		}
		if evidence.WorkerWait.Result == LifetimeWorkerWaitAbsent {
			if evidence.Registration != nil {
				return LifetimeClosure{Kind: LifetimeClosureRegisteredRecovered}, nil
			}
			return LifetimeClosure{Kind: LifetimeClosureUnregisteredRecovered}, nil
		}
		if evidence.Termination == nil {
			return LifetimeClosure{Kind: LifetimeClosureOpen}, nil
		}
		return LifetimeClosure{Kind: LifetimeClosureRegisteredNative}, nil
	}
	if evidence.Termination != nil && evidence.Termination.Cause == LifetimeTerminationPreviousBoot {
		if evidence.Registration == nil {
			return LifetimeClosure{}, lifetimeEvidenceFault(ErrIdentityMismatch)
		}
		return LifetimeClosure{Kind: LifetimeClosureLegacyPreviousBoot}, nil
	}
	return LifetimeClosure{Kind: LifetimeClosureOpen}, nil
}

// NewLifetimeWorkerAbsence builds the canonical absence completion candidate
// from typed records. Native observation and owner authorization happen before
// this pure factory is called; it never queries the operating system.
func NewLifetimeWorkerAbsence(evidence LifetimeEvidence, proof LifetimeAbsenceProof, observedBootID string, observedAt time.Time) (LifetimeWorkerWaitRecord, error) {
	var record LifetimeWorkerWaitRecord
	if observedAt.IsZero() {
		return record, errors.New("lifetime absence observation time is zero")
	}
	if evidence.WorkerWait != nil {
		return record, ErrIdentityMismatch
	}
	if err := validateLifetimeEvidenceFacts(evidence); err != nil {
		return record, err
	}
	if !validLifetimeToken(observedBootID) {
		return record, ErrIdentityMismatch
	}

	registration := evidence.Registration
	intent := evidence.Intent
	worker := evidence.Worker
	if registration == nil && intent == nil {
		return record, ErrIdentityMismatch
	}

	record.SchemaVersion = SchemaVersion
	if registration != nil {
		registrationDigest, err := lifetimeCompletionDigest(registration)
		if err != nil {
			return record, err
		}
		record.RootID = registration.RootID
		record.TaskID = registration.TaskID
		record.RegistrationSHA256 = registrationDigest
	} else {
		intentDigest, err := lifetimeCompletionDigest(intent)
		if err != nil {
			return record, err
		}
		record.RootID = intent.RootID
		record.TaskID = intent.TaskID
		record.IntentSHA256 = intentDigest
	}
	record.Result = LifetimeWorkerWaitAbsent
	record.AbsenceProof = proof
	record.ObservedBootID = observedBootID
	record.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
	if worker != nil {
		workerDigest, err := lifetimeCompletionDigest(worker)
		if err != nil {
			return record, err
		}
		record.WorkerSHA256 = workerDigest
		record.ChildPID = worker.PID
	}
	if err := ValidateLifetimeWorkerWaitRecord(&record); err != nil {
		return record, err
	}
	evidence.WorkerWait = &record
	if err := ValidateLifetimeCompletionBinding(&record, evidence); err != nil {
		return record, err
	}
	return record, nil
}

func validateLifetimeWorkerWaitAbsenceFields(record *LifetimeWorkerWaitRecord) error {
	if record.Result != LifetimeWorkerWaitAbsent {
		if record.IntentSHA256 != "" || record.AbsenceProof != "" || record.ObservedBootID != "" {
			return ErrIdentityMismatch
		}
		return nil
	}
	if record.RegistrationSHA256 == "" && record.IntentSHA256 == "" {
		return ErrIdentityMismatch
	}
	if record.RegistrationSHA256 != "" && record.IntentSHA256 != "" {
		return ErrIdentityMismatch
	}
	if record.RegistrationSHA256 != "" {
		if err := ValidateSHA256(record.RegistrationSHA256); err != nil {
			return err
		}
	}
	if record.IntentSHA256 != "" {
		if err := ValidateSHA256(record.IntentSHA256); err != nil {
			return err
		}
	}
	if !validLifetimeToken(record.ObservedBootID) {
		return ErrIdentityMismatch
	}
	switch record.AbsenceProof {
	case LifetimeAbsenceExactWorker, LifetimeAbsencePreviousBoot, LifetimeAbsenceUnregisteredDisposed:
	default:
		return fmt.Errorf("invalid lifetime absence proof: %w", ErrInvalidEnum)
	}
	return nil
}

func validateLifetimeWorkerWaitAbsenceResult(record *LifetimeWorkerWaitRecord) error {
	if record.ExitCode != nil || (record.WorkerSHA256 == "" && record.ChildPID != 0) ||
		(record.WorkerSHA256 != "" && record.ChildPID <= 0) {
		return ErrIdentityMismatch
	}
	switch record.AbsenceProof {
	case LifetimeAbsenceExactWorker:
		if record.RegistrationSHA256 == "" || record.WorkerSHA256 == "" {
			return ErrIdentityMismatch
		}
	case LifetimeAbsencePreviousBoot:
		if record.IntentSHA256 != "" && record.WorkerSHA256 != "" {
			return ErrIdentityMismatch
		}
	case LifetimeAbsenceUnregisteredDisposed:
		if record.IntentSHA256 == "" || record.WorkerSHA256 != "" {
			return ErrIdentityMismatch
		}
	default:
		return fmt.Errorf("invalid lifetime absence proof: %w", ErrInvalidEnum)
	}
	return nil
}

func validateLifetimeAbsenceBinding(record *LifetimeWorkerWaitRecord, evidence LifetimeEvidence, registration *LifetimeRegistrationRecord, intent *LifetimeIntentRecord, worker *LifetimeWorkerRecord) error {
	if registration != nil && intent != nil && !LifetimeIntentMatchesRegistration(intent, registration) {
		return ErrIdentityMismatch
	}
	switch record.AbsenceProof {
	case LifetimeAbsenceExactWorker:
		return validateLifetimeExactWorkerAbsenceBinding(record, evidence.Termination, registration, worker)
	case LifetimeAbsencePreviousBoot:
		if registration != nil {
			return validateLifetimeRegisteredPreviousBootAbsenceBinding(record, evidence.Termination, registration, worker)
		}
		return validateLifetimeIntentPreviousBootAbsenceBinding(record, evidence, intent, worker)
	case LifetimeAbsenceUnregisteredDisposed:
		return validateLifetimeUnregisteredAbsenceBinding(record, evidence, intent, registration, worker)
	default:
		return fmt.Errorf("invalid lifetime absence proof: %w", ErrInvalidEnum)
	}
}

func validateLifetimeExactWorkerAbsenceBinding(record *LifetimeWorkerWaitRecord, termination *LifetimeTerminationRecord, registration *LifetimeRegistrationRecord, worker *LifetimeWorkerRecord) error {
	if registration == nil || worker == nil || termination == nil || record.IntentSHA256 != "" {
		return ErrIdentityMismatch
	}
	registrationDigest, err := lifetimeCompletionDigest(registration)
	if err != nil {
		return err
	}
	workerDigest, err := lifetimeCompletionDigest(worker)
	if err != nil {
		return err
	}
	if record.RegistrationSHA256 != registrationDigest || record.ObservedBootID != registration.BootID {
		return ErrIdentityMismatch
	}
	if worker.BootID != registration.BootID || !LifetimeWorkerWaitMatchesWorker(record, worker, workerDigest) {
		return ErrIdentityMismatch
	}
	if termination.Cause == LifetimeTerminationPreviousBoot {
		return ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeRegisteredPreviousBootAbsenceBinding(record *LifetimeWorkerWaitRecord, termination *LifetimeTerminationRecord, registration *LifetimeRegistrationRecord, worker *LifetimeWorkerRecord) error {
	if termination == nil {
		return ErrIdentityMismatch
	}
	registrationDigest, err := lifetimeCompletionDigest(registration)
	if err != nil {
		return err
	}
	if record.RegistrationSHA256 != registrationDigest || record.ObservedBootID == registration.BootID {
		return ErrIdentityMismatch
	}
	if worker == nil {
		if record.WorkerSHA256 != "" || record.ChildPID != 0 {
			return ErrIdentityMismatch
		}
		return nil
	}
	workerDigest, err := lifetimeCompletionDigest(worker)
	if err != nil {
		return err
	}
	if worker.BootID != registration.BootID || !LifetimeWorkerWaitMatchesWorker(record, worker, workerDigest) {
		return ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeIntentPreviousBootAbsenceBinding(record *LifetimeWorkerWaitRecord, evidence LifetimeEvidence, intent *LifetimeIntentRecord, worker *LifetimeWorkerRecord) error {
	if intent == nil || record.IntentSHA256 == "" || evidence.Termination != nil || worker != nil || len(evidence.Launches) != 0 || record.WorkerSHA256 != "" || record.ChildPID != 0 || record.ObservedBootID == intent.BootID {
		return ErrIdentityMismatch
	}
	intentDigest, err := lifetimeCompletionDigest(intent)
	if err != nil {
		return err
	}
	if record.IntentSHA256 != intentDigest {
		return ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeUnregisteredAbsenceBinding(record *LifetimeWorkerWaitRecord, evidence LifetimeEvidence, intent *LifetimeIntentRecord, registration *LifetimeRegistrationRecord, worker *LifetimeWorkerRecord) error {
	if intent == nil || registration != nil || worker != nil || len(evidence.Launches) != 0 || evidence.Termination != nil || record.RegistrationSHA256 != "" || record.ObservedBootID != intent.BootID {
		return ErrIdentityMismatch
	}
	intentDigest, err := lifetimeCompletionDigest(intent)
	if err != nil {
		return err
	}
	if record.IntentSHA256 != intentDigest {
		return ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeEvidenceFacts(evidence LifetimeEvidence) error {
	if err := validateLifetimeEvidenceRecords(evidence); err != nil {
		return err
	}
	if err := validateLifetimeEvidenceBindings(evidence); err != nil {
		return err
	}
	return nil
}

type lifetimeEvidenceIdentity struct {
	rootID string
	taskID string
}

func (identity *lifetimeEvidenceIdentity) set(rootID, taskID string) error {
	if identity.rootID == "" {
		identity.rootID = rootID
		identity.taskID = taskID
		return nil
	}
	if identity.rootID != rootID || identity.taskID != taskID {
		return ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeScopeEvidence(evidence LifetimeEvidence, identity *lifetimeEvidenceIdentity) error {
	if evidence.Intent != nil {
		if err := ValidateLifetimeIntentRecord(evidence.Intent); err != nil {
			return err
		}
		if err := identity.set(evidence.Intent.RootID, evidence.Intent.TaskID); err != nil {
			return err
		}
	}
	if evidence.Registration != nil {
		if err := ValidateLifetimeRegistrationRecord(evidence.Registration); err != nil {
			return err
		}
		if err := identity.set(evidence.Registration.RootID, evidence.Registration.TaskID); err != nil {
			return err
		}
	}
	return nil
}

func validateLifetimeEvidenceRecords(evidence LifetimeEvidence) error {
	identity := lifetimeEvidenceIdentity{}
	if err := validateLifetimeScopeEvidence(evidence, &identity); err != nil {
		return err
	}
	if evidence.Worker != nil {
		if err := ValidateLifetimeWorkerRecord(evidence.Worker); err != nil {
			return err
		}
		if err := identity.set(evidence.Worker.RootID, evidence.Worker.TaskID); err != nil {
			return err
		}
	}
	if evidence.WorkerWait != nil {
		if err := ValidateLifetimeWorkerWaitRecord(evidence.WorkerWait); err != nil {
			return err
		}
		if err := identity.set(evidence.WorkerWait.RootID, evidence.WorkerWait.TaskID); err != nil {
			return err
		}
	}
	if evidence.Termination != nil {
		if err := ValidateLifetimeTerminationRecord(evidence.Termination); err != nil {
			return err
		}
		if err := identity.set(evidence.Termination.RootID, evidence.Termination.TaskID); err != nil {
			return err
		}
	}
	if uint64(len(evidence.Launches)) > MaxLifetimeLaunches {
		return fmt.Errorf("%w: too many lifetime launches", ErrEvidenceFault)
	}
	return validateLifetimeEvidenceLaunches(evidence, identity)
}

func validateLifetimeEvidenceLaunches(evidence LifetimeEvidence, identity lifetimeEvidenceIdentity) error {
	for index := range evidence.Launches {
		launch := &evidence.Launches[index]
		if err := ValidateLifetimeLaunchRecord(launch); err != nil {
			return err
		}
		if launch.Sequence != uint64(index+1) {
			return ErrIdentityMismatch
		}
		if err := identity.set(launch.RootID, launch.TaskID); err != nil {
			return err
		}
	}
	return nil
}

func validateLifetimeEvidenceBindings(evidence LifetimeEvidence) error {
	if evidence.Registration == nil && (evidence.Worker != nil || len(evidence.Launches) != 0 || evidence.Termination != nil) {
		return ErrIdentityMismatch
	}
	if evidence.Intent != nil && evidence.Registration != nil && !LifetimeIntentMatchesRegistration(evidence.Intent, evidence.Registration) {
		return ErrIdentityMismatch
	}
	if evidence.Registration != nil {
		registrationDigest, err := lifetimeCompletionDigest(evidence.Registration)
		if err != nil {
			return err
		}
		if err = validateLifetimeRegisteredEvidenceBindings(evidence, registrationDigest); err != nil {
			return err
		}
	}
	return validateLifetimeEvidenceTermination(evidence)
}

func validateLifetimeRegisteredEvidenceBindings(evidence LifetimeEvidence, registrationDigest string) error {
	if evidence.Worker != nil && (evidence.Worker.RegistrationSHA256 != registrationDigest || evidence.Worker.BootID != evidence.Registration.BootID) {
		return ErrIdentityMismatch
	}
	for index := range evidence.Launches {
		if evidence.Launches[index].RegistrationSHA256 != registrationDigest {
			return ErrIdentityMismatch
		}
	}
	if evidence.Termination != nil && evidence.Termination.RegistrationSHA256 != registrationDigest {
		return ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeEvidenceTermination(evidence LifetimeEvidence) error {
	if evidence.Termination == nil {
		return nil
	}
	launchSetDigest, err := lifetimeLaunchSetDigest(evidence.Launches)
	if err != nil {
		return err
	}
	if evidence.Termination.LaunchCount != uint64(len(evidence.Launches)) || evidence.Termination.LaunchSetSHA256 != launchSetDigest {
		return ErrIdentityMismatch
	}
	return nil
}

type lifetimeCompletionDigestRecord interface {
	*LifetimeIntentRecord | *LifetimeRegistrationRecord | *LifetimeWorkerRecord | *LifetimeWorkerWaitRecord
}

func lifetimeCompletionDigest[T lifetimeCompletionDigestRecord](record T) (string, error) {
	data, err := MarshalCanonical(record)
	if err != nil {
		return "", err
	}
	return ComputeSHA256(data), nil
}

func lifetimeLaunchSetDigest(launches []LifetimeLaunchRecord) (string, error) {
	if launches == nil {
		launches = make([]LifetimeLaunchRecord, 0)
	}
	data, err := MarshalCanonical(launches)
	if err != nil {
		return "", err
	}
	return ComputeSHA256(data), nil
}

func lifetimeEvidenceFault(err error) error {
	if err == nil || errors.Is(err, ErrEvidenceFault) {
		return err
	}
	return errors.Join(ErrEvidenceFault, err)
}

// LifetimeWorkerWaitProvesAbsence identifies the recovered completion variant.
// Its store reader validates the witness and dependencies before it is used.
func LifetimeWorkerWaitProvesAbsence(record *LifetimeWorkerWaitRecord) bool {
	return record != nil && record.Result == LifetimeWorkerWaitAbsent
}
