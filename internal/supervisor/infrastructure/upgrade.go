package infrastructure

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These values are projections of the upgrade owner's canonical journal. They
// validate boundary data; they neither select a successor nor advance a phase.
type UpgradeOperationID string

func ParseUpgradeOperationID(value string) (UpgradeOperationID, error) {
	const prefix = "upgrade-v1:"
	if !strings.HasPrefix(value, prefix) || task.ValidateTaskID(strings.TrimPrefix(value, prefix)) != nil {
		return "", ErrIdentity
	}
	return UpgradeOperationID(value), nil
}

type UpgradeAction string

const (
	UpgradeStatus   UpgradeAction = "upgrade-status"
	UpgradeEnsure   UpgradeAction = "upgrade-ensure"
	UpgradePrepare  UpgradeAction = "upgrade-prepare"
	UpgradeComplete UpgradeAction = "upgrade-complete"
)

type UpgradeRequest struct {
	Action      UpgradeAction          `json:"action"`
	OperationID UpgradeOperationID     `json:"upgrade_operation_id,omitzero"`
	Context     LaunchContextReference `json:"context"`
}

func (r UpgradeRequest) Validate() error {
	if err := r.Context.Validate(); err != nil {
		return err
	}
	switch r.Action {
	case UpgradeStatus, UpgradeEnsure, UpgradePrepare:
		if r.OperationID != "" {
			return ErrIdentity
		}
	case UpgradeComplete:
		if _, err := ParseUpgradeOperationID(string(r.OperationID)); err != nil {
			return err
		}
	default:
		return ErrIdentity
	}
	return nil
}

type UpgradePreflightState string

const (
	UpgradePreflightPending UpgradePreflightState = "pending"
	UpgradePreflightPassed  UpgradePreflightState = "passed"
	UpgradePreflightFailed  UpgradePreflightState = "failed"
)

// Errors remain with the owner. The wire carries only a bounded reason code,
// never a filesystem/native diagnostic or launch-context contents.
type UpgradePreflight struct {
	State      UpgradePreflightState `json:"state"`
	ReasonCode string                `json:"reason_code,omitempty"`
}

func (p UpgradePreflight) Validate() error {
	switch p.State {
	case UpgradePreflightPending, UpgradePreflightPassed:
		if p.ReasonCode != "" {
			return ErrIdentity
		}
	case UpgradePreflightFailed:
		if !validUpgradeReason(p.ReasonCode) {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	return nil
}

type UpgradeMode string

const (
	UpgradeReactive          UpgradeMode = "reactive"
	UpgradeOperationRequired UpgradeMode = "operation_required"
)

type UpgradePhase string

const (
	UpgradeDraining             UpgradePhase = "draining"
	UpgradeAwaitingInstallation UpgradePhase = "awaiting_installation"
	UpgradeAuthorized           UpgradePhase = "authorized"
	UpgradeMigrating            UpgradePhase = "migrating"
)

type UpgradeReport struct {
	OperationID UpgradeOperationID `json:"operation_id"`
	Mode        UpgradeMode        `json:"mode"`
	Phase       UpgradePhase       `json:"phase"`
	Predecessor task.SupervisorRef `json:"predecessor"`
	Completed   bool               `json:"completed,omitempty"`
}

func (r UpgradeReport) Validate() error {
	if r == (UpgradeReport{}) {
		return nil
	}
	if _, err := ParseUpgradeOperationID(string(r.OperationID)); err != nil {
		return err
	}
	switch r.Mode {
	case UpgradeReactive, UpgradeOperationRequired:
	default:
		return ErrIdentity
	}
	switch r.Phase {
	case UpgradeDraining, UpgradeAuthorized, UpgradeMigrating:
	case UpgradeAwaitingInstallation:
		if r.Mode != UpgradeOperationRequired {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	if r.Completed && r.Phase != UpgradeMigrating {
		return ErrIdentity
	}
	if r.Predecessor == (task.SupervisorRef{}) {
		return ErrIdentity
	}
	return task.ValidateSupervisorRef(r.Predecessor)
}

type UpgradeCondition string

const (
	UpgradeOK          UpgradeCondition = "ok"
	UpgradePending     UpgradeCondition = "pending"
	UpgradeBusy        UpgradeCondition = "busy"
	UpgradeRefused     UpgradeCondition = "refused"
	UpgradeUnsupported UpgradeCondition = "unsupported"
	UpgradeFault       UpgradeCondition = "fault"
	UpgradeTimeout     UpgradeCondition = "timeout"
)

// UpgradeOutcome is a verified observation independent of transport cleanup.
// Zero means no verified reply. Status carries only capability/preflight facts.
type UpgradeOutcome struct {
	Condition   UpgradeCondition   `json:"condition"`
	Preflight   UpgradePreflight   `json:"preflight"`
	Report      UpgradeReport      `json:"report,omitzero"`
	Disposition Disposition        `json:"disposition,omitzero"`
	ReasonCode  string             `json:"reason_code,omitempty"`
	FailureFact UpgradeFailureFact `json:"failure_fact,omitzero"`
}

// UpgradeFailureFact is a diagnostic projection of the hook owner's named
// error. The original error graph remains with that owner. This value grants
// no mutation, retry, admission or lifecycle authority.
type UpgradeFailureFact struct {
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
	FactKind     string `json:"fact_kind"`
	Name         string `json:"name"`
	ReasonCode   string `json:"reason_code"`
	NextAction   string `json:"next_action"`
}

func (f UpgradeFailureFact) Validate() error {
	if f == (UpgradeFailureFact{}) {
		return nil
	}
	if !validUpgradeReason(f.ReasonCode) || !validUpgradeReason(f.NextAction) {
		return ErrIdentity
	}
	if err := f.validateResource(); err != nil {
		return err
	}
	return f.validateName()
}

func (f UpgradeFailureFact) validateResource() error {
	switch f.ResourceKind {
	case "root":
		return task.ValidateRootID(f.ResourceID)
	case "task", "inspection", "workspace_operation":
		if task.ValidateTaskID(f.ResourceID) != nil {
			return ErrIdentity
		}
	case "supervisor":
		switch f.ResourceID {
		case "recorded_client", "recorded_daemon", "selected_client", "selected_daemon":
		default:
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	return nil
}

func (f UpgradeFailureFact) validateName() error {
	if f.Name == "" || len(f.Name) > 4096 || !utf8.ValidString(f.Name) || strings.IndexFunc(f.Name, unicode.IsControl) >= 0 {
		return ErrIdentity
	}
	switch f.FactKind {
	case "executable":
		if f.ResourceKind != "supervisor" || !filepath.IsAbs(f.Name) || filepath.Clean(f.Name) != f.Name {
			return ErrIdentity
		}
	case "evidence", "evidence_record", "evidence_file":
		if len(f.Name) > 256 || !filepath.IsLocal(f.Name) {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	return nil
}

func (o UpgradeOutcome) Validate() error {
	if o == (UpgradeOutcome{}) {
		return nil
	}
	switch o.Condition {
	case UpgradeOK, UpgradePending, UpgradeBusy, UpgradeRefused, UpgradeUnsupported, UpgradeFault, UpgradeTimeout:
	default:
		return ErrIdentity
	}
	if err := errors.Join(o.Preflight.Validate(), o.Report.Validate()); err != nil {
		return err
	}
	if o.Condition == UpgradeOK && o.Preflight.State != UpgradePreflightPassed {
		return ErrIdentity
	}
	if o.Preflight.State != UpgradePreflightPassed && (o.Report != (UpgradeReport{}) || o.Disposition != (Disposition{})) {
		return ErrIdentity
	}
	if o.Disposition != (Disposition{}) {
		if err := o.Disposition.Validate(); err != nil {
			return err
		}
	}
	if o.ReasonCode != "" && !validUpgradeReason(o.ReasonCode) {
		return ErrIdentity
	}
	return o.validateFailureFact()
}

func (o UpgradeOutcome) validateFailureFact() error {
	if o.FailureFact != (UpgradeFailureFact{}) && (o.Condition != UpgradeFault || o.Preflight.State != UpgradePreflightPassed) {
		return ErrIdentity
	}
	return o.FailureFact.Validate()
}

func validUpgradeReason(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, character := range value {
		if character != '_' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// UpgradeHost is an optional port owned by the infrastructure consumer. Trusted
// app composition owns eligibility, canonical Drain transitions and filesystem
// support validation after the authenticated explicit action. The server owns
// authentication and the initial gate, and never supplies transition policy.
// Handled pending/refusal/timeout is encoded in Outcome with nil error. Any
// nonnil error is an unhandled or cleanup fault: the server forces UpgradeFault
// while preserving a valid progress report and the complete private error graph.
type UpgradeHost interface {
	UpgradeOwnRoot(context.Context, Operation, LaunchContext, UpgradeRequest, ResidentGuards, DaemonWaitOwner) (UpgradeOutcome, error)
}

func validateUpgradePreflightBirth(intent StartupIntent, environment []string) error {
	if intent.Inputs.Initial.Kind != InitialUpgradePreflight {
		return nil
	}
	if intent.Inputs.RuntimeProducer != producerOutside {
		return ErrOwnerRequest
	}
	_, found, err := ReadOrigin(environment)
	if err != nil || found {
		return ErrOwnerRequest
	}
	return nil
}
