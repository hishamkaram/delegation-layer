package task

import (
	"errors"
	"fmt"
)

func validateBoundRecord(schema int, root, id, spec, meta string) error {
	if schema != SchemaVersion {
		return ErrUnsupportedSchema
	}
	for _, err := range []error{ValidateRootID(root), ValidateTaskID(id), ValidateSHA256(spec), ValidateSHA256(meta)} {
		if err != nil {
			return err
		}
	}
	return nil
}

func ValidateSupervisorReceipt(r *SupervisorReceipt) error {
	if r == nil {
		return errors.New("nil supervisor receipt")
	}
	if err := validateBoundRecord(r.SchemaVersion, r.RootID, r.TaskID, r.SpecSHA256, r.MetaSHA256); err != nil {
		return err
	}
	if r.NumericTaskID < 0 || r.Label != "delegate:"+r.RootID+":"+r.TaskID {
		return ErrIdentityMismatch
	}
	return ValidateFreshSupervisorRef(r.SupervisorRef())
}

func (r SupervisorReceipt) SupervisorRef() SupervisorRef {
	return SupervisorRef{ConfigPath: r.ConfigPath, Endpoint: r.Endpoint, ConfigDigest: r.ConfigDigest, ObservedVersion: r.ObservedVersion, ClientExecutable: r.ClientExecutable, ClientSHA256: r.ClientSHA256, DaemonExecutable: r.DaemonExecutable, DaemonSHA256: r.DaemonSHA256, ResolutionOS: r.ResolutionOS, ResolutionHome: r.ResolutionHome, ResolutionDataLocal: r.ResolutionDataLocal, ResolutionConfig: r.ResolutionConfig, ResolutionRuntime: r.ResolutionRuntime, ResolutionUsername: r.ResolutionUsername, ResolutionCwd: r.ResolutionCwd, ResolvedConfigSHA256: r.ResolvedConfigSHA256}
}

func ValidateProviderRefRecord(r *ProviderRefRecord) error {
	if r == nil {
		return errors.New("nil provider ref")
	}
	if err := validateBoundRecord(r.SchemaVersion, r.RootID, r.TaskID, r.SpecSHA256, r.MetaSHA256); err != nil {
		return err
	}
	if err := ValidateSessionIdentity(SessionIdentity{Provider: r.Provider, ConversationID: r.ConversationID}); err != nil {
		return err
	}
	return validateTimestamp(r.ObservedAt)
}

func ValidateStopRequestID(id string) error {
	if id == "budget" {
		return nil
	}
	return ValidateTaskID(id)
}

func ValidateStopRequestRecord(r *StopRequestRecord) error {
	if r == nil {
		return errors.New("nil stop request")
	}
	if err := validateBoundRecord(r.SchemaVersion, r.RootID, r.TaskID, r.SpecSHA256, r.MetaSHA256); err != nil {
		return err
	}
	if err := ValidateStopRequestID(r.RequestID); err != nil {
		return err
	}
	if r.NumericTaskID != nil && *r.NumericTaskID < 0 {
		return ErrIdentityMismatch
	}
	if r.Label != "delegate:"+r.RootID+":"+r.TaskID || r.BudgetNanos <= 0 {
		return ErrIdentityMismatch
	}
	if err := ValidateFreshSupervisorRef(r.Supervisor); err != nil {
		return err
	}
	if err := validateStopDeadline(r); err != nil {
		return err
	}
	return validateTimestamp(r.RequestedAt)
}

func validateStopDeadline(r *StopRequestRecord) error {
	switch r.Cause {
	case "budget":
		if r.RequestID != "budget" {
			return ErrIdentityMismatch
		}
		return validateTimestamp(r.Deadline)
	case "user":
		if r.RequestID == "budget" || r.Deadline != "" {
			return ErrIdentityMismatch
		}
		return nil
	default:
		return ErrInvalidEnum
	}
}

func validateStopEvidence(schema int, root, id, spec, meta, label, digest string, ref *SupervisorRef, numeric *int64) error {
	if err := validateBoundRecord(schema, root, id, spec, meta); err != nil {
		return err
	}
	if label != "delegate:"+root+":"+id || ref == nil || numeric == nil || *numeric < 0 {
		return ErrIdentityMismatch
	}
	if err := ValidateFreshSupervisorRef(*ref); err != nil {
		return err
	}
	return ValidateSHA256(digest)
}

func ValidateStopReplyRecord(r *StopReplyRecord) error {
	if r == nil {
		return errors.New("nil stop reply")
	}
	if err := validateStopEvidence(r.SchemaVersion, r.RootID, r.TaskID, r.SpecSHA256, r.MetaSHA256, r.Label, r.RequestSHA256, r.Supervisor, r.NumericTaskID); err != nil {
		return err
	}
	if err := ValidateStopRequestID(r.RequestID); err != nil {
		return err
	}
	if r.Action != "kill" && r.Action != "remove" {
		return fmt.Errorf("invalid stop action: %w", ErrInvalidEnum)
	}
	return validateTimestamp(r.RepliedAt)
}

func ValidateStopObservedRecord(r *StopObservedRecord) error {
	if r == nil {
		return errors.New("nil stop observation")
	}
	if err := validateStopEvidence(r.SchemaVersion, r.RootID, r.TaskID, r.SpecSHA256, r.MetaSHA256, r.Label, r.RequestSHA256, r.Supervisor, r.NumericTaskID); err != nil {
		return err
	}
	if err := ValidateStopRequestID(r.RequestID); err != nil {
		return err
	}
	switch r.State {
	case "queued", "running", "ended":
	default:
		return ErrInvalidEnum
	}
	if r.Terminated != (r.State == "ended") {
		return ErrIdentityMismatch
	}
	return validateTimestamp(r.ObservedAt)
}
