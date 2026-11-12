package infrastructure

import "errors"

type PreparationCondition string

const (
	PreparationOK           PreparationCondition = "ok"
	PreparationPending      PreparationCondition = "pending"
	PreparationBusy         PreparationCondition = "busy"
	PreparationIncompatible PreparationCondition = "incompatible"
	PreparationRefused      PreparationCondition = "refused"
	PreparationFault        PreparationCondition = "fault"
	PreparationTimeout      PreparationCondition = "timeout"
)

func (c PreparationCondition) Validate() error {
	switch c {
	case PreparationOK, PreparationPending, PreparationBusy, PreparationIncompatible,
		PreparationRefused, PreparationFault, PreparationTimeout:
		return nil
	default:
		return ErrIdentity
	}
}

// PreparationOutcome retains one immutable verified reply, independently of
// transport and Close errors returned by its consumer. Zero means no verified
// reply. References and observations do not certify live readiness.
type PreparationOutcome struct {
	Condition      PreparationCondition  `json:"condition,omitempty"`
	Startup        StartupReference      `json:"startup,omitzero"`
	Disposition    Disposition           `json:"disposition,omitzero"`
	Report         ManualMigrationReport `json:"report,omitzero"`
	RuntimeRestart RuntimeRestartReport  `json:"runtime_restart,omitzero"`
	Failure        PreparationFailure    `json:"failure,omitzero"`
}

// Validate checks data shape and the shared report table, not action-specific
// success requirements. Those belong to the wire operation's consumer.
func (o PreparationOutcome) Validate() error {
	if o == (PreparationOutcome{}) {
		return nil
	}
	if err := o.Condition.Validate(); err != nil {
		return err
	}
	if o.Startup != (StartupReference{}) {
		if err := o.Startup.Validate(); err != nil {
			return err
		}
	}
	if o.Disposition != (Disposition{}) {
		if err := o.Disposition.Validate(); err != nil {
			return err
		}
	}
	if err := o.Failure.Validate(); err != nil {
		return err
	}
	if o.RuntimeRestart != (RuntimeRestartReport{}) {
		if err := o.RuntimeRestart.ValidateDisposition(o.Disposition); err != nil {
			return err
		}
	}
	return errors.Join(o.Report.ValidateCondition(o.Condition), o.RuntimeRestart.ValidateCondition(o.Condition))
}

// PreparationRequest carries one purpose, its context selection and an optional
// initial-result selector. The original intent owns purpose; the selector only
// asks for that owner's retained outcome and never authorizes a host operation.
type PreparationRequest struct {
	Operation        InitialOperation       `json:"operation,omitzero"`
	Context          LaunchContextReference `json:"context,omitzero"`
	InitialResultFor StartupReference       `json:"initial_result_for,omitzero"`
}

// Validate runs before dial against the independently selected expected owner.
// A zero context selects that owner's recorded default context.
func (r PreparationRequest) Validate(rootID string, expected StartupReference) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := r.Operation.Validate(rootID); err != nil {
		return err
	}
	if r.Context != (LaunchContextReference{}) {
		if err := r.Context.Validate(); err != nil {
			return err
		}
	}
	if r.InitialResultFor != (StartupReference{}) && r.InitialResultFor != expected {
		return ErrIdentity
	}
	return nil
}

// Normalized returns a validated copy, preserving the caller's immutable data.
func (r PreparationRequest) Normalized(rootID string, expected StartupReference) (PreparationRequest, error) {
	if err := r.Validate(rootID, expected); err != nil {
		return PreparationRequest{}, err
	}
	r.Operation = r.Operation.normalized()
	return r, nil
}

// EstablishedOwner is data returned by establishment. Its nonzero Owner names
// the attempt to contact; InitialResultFor, when set, selects that same attempt.
type EstablishedOwner struct {
	Owner            StartupReference `json:"owner"`
	InitialResultFor StartupReference `json:"initial_result_for,omitzero"`
}

func (o EstablishedOwner) Validate() error {
	if err := o.Owner.Validate(); err != nil {
		return err
	}
	if o.InitialResultFor != (StartupReference{}) && o.InitialResultFor != o.Owner {
		return ErrIdentity
	}
	return nil
}
