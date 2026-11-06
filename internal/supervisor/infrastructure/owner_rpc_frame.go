package infrastructure

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Owner wire compatibility evolves separately from persisted record schemas.
const ownerProtocolVersion = 4

type ownerAction string

const (
	ownerStatus              ownerAction = "status"
	ownerEnsureOwn           ownerAction = "ensure-own"
	ownerEnsureOther         ownerAction = "ensure-other"
	ownerRecoverSaved        ownerAction = "recover-saved"
	ownerPrepareContinuation ownerAction = "prepare-continuation"
	ownerMigrateOwn          ownerAction = "migrate-own"
)

type ownerReplyCode string

const (
	ownerOK           ownerReplyCode = "ok"
	ownerPending      ownerReplyCode = "pending"
	ownerBusy         ownerReplyCode = "busy"
	ownerIncompatible ownerReplyCode = "incompatible"
	ownerRefused      ownerReplyCode = "refused"
	ownerFault        ownerReplyCode = "fault"
	ownerTimeout      ownerReplyCode = "timeout"
)

// Requests name the expected immutable publication, never a new mutable head.
// These values establish shape only; the server authenticates the peer and
// checks the publication, process, target pins and finite deadline against reality.
type ownerRequest struct {
	Protocol          int                    `json:"protocol"`
	RootID            string                 `json:"root_id"`
	Owner             StartupReference       `json:"owner"`
	PublicationSHA256 string                 `json:"publication_sha256"`
	Client            ProcessIdentity        `json:"client"`
	Action            ownerAction            `json:"action"`
	Target            originRecord           `json:"target"`
	Context           LaunchContextReference `json:"context"`
	Deadline          string                 `json:"deadline"`
	Source            SavedBindingSource     `json:"source"`
	Initial           InitialOperation       `json:"initial_operation,omitzero"`
	InitialResultFor  StartupReference       `json:"initial_result_for,omitzero"`
}

func (r ownerRequest) validate() error {
	if err := validateOwnerSubject(r.Protocol, r.RootID, r.Owner, r.PublicationSHA256); err != nil {
		return err
	}
	if _, err := r.Client.Reference(); err != nil {
		return err
	}
	deadline, err := canonicalStartupTime(r.Deadline)
	if err != nil {
		return err
	}
	if deadline.IsZero() {
		return ErrIdentity
	}
	return r.validateOperationSubject()
}

// Every operation has one immutable subject. Zero context means the receiving
// owner's recorded default; other-root birth always names the target context.
// These locators are data. The resident must re-read their canonical owners.
func (r ownerRequest) validateSelections() error {
	if r.InitialResultFor != (StartupReference{}) && (!ownPreparationAction(r.Action) || r.InitialResultFor != r.Owner) {
		return ErrIdentity
	}
	if r.Context != (LaunchContextReference{}) {
		if err := r.Context.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r ownerRequest) validateOperationSubject() error {
	if err := r.validateSelections(); err != nil {
		return err
	}
	if err := r.rejectForeignSubjects(); err != nil {
		return err
	}
	switch r.Action {
	case ownerStatus:
		if r.Context != (LaunchContextReference{}) {
			return ErrIdentity
		}
	case ownerEnsureOwn, ownerMigrateOwn:
	case ownerEnsureOther:
		return errors.Join(validateOwnerTarget(r.Target, r.RootID), r.Context.Validate(), r.Initial.Validate(r.Target.RootID))
	case ownerPrepareContinuation:
		return (InitialOperation{Kind: InitialPrepareContinuation, Source: r.Source}).Validate(r.RootID)
	case ownerRecoverSaved:
		if r.Source.RootID != r.RootID {
			return ErrIdentity
		}
		return r.Source.Validate()
	default:
		return ErrIdentity
	}
	return nil
}

func (r ownerRequest) rejectForeignSubjects() error {
	for _, subject := range []struct {
		present bool
		action  ownerAction
	}{
		{r.Source != (SavedBindingSource{}) && r.Action != ownerPrepareContinuation, ownerRecoverSaved},
		{r.Target != (originRecord{}), ownerEnsureOther},
		{r.Initial != (InitialOperation{}), ownerEnsureOther},
	} {
		if subject.present && r.Action != subject.action {
			return ErrIdentity
		}
	}
	return nil
}

func ownPreparationAction(action ownerAction) bool {
	switch action {
	case ownerEnsureOwn, ownerRecoverSaved, ownerPrepareContinuation, ownerMigrateOwn:
		return true
	case ownerStatus, ownerEnsureOther:
		return false
	}
	return false
}

// Replies echo a subject and finite outcome. Their references are not evidence
// of live readiness: callers must compare the requested subject and consult the
// actual publication/startup owner before treating an outcome as operational.
type ownerReply struct {
	Protocol          int                    `json:"protocol"`
	RootID            string                 `json:"root_id"`
	OwnerRef          StartupReference       `json:"owner_ref"`
	PublicationSHA256 string                 `json:"publication_sha256"`
	Action            ownerAction            `json:"action"`
	Target            originRecord           `json:"target"`
	Startup           StartupReference       `json:"startup"`
	Code              ownerReplyCode         `json:"code"`
	Context           LaunchContextReference `json:"context"`
	Source            SavedBindingSource     `json:"source"`
	Initial           InitialOperation       `json:"initial_operation,omitzero"`
	InitialResultFor  StartupReference       `json:"initial_result_for,omitzero"`
	Disposition       Disposition            `json:"disposition"`
	Report            ManualMigrationReport  `json:"report,omitzero"`
	Failure           PreparationFailure     `json:"failure,omitzero"`
}

func (r ownerReply) validate() error {
	if err := validateOwnerSubject(r.Protocol, r.RootID, r.OwnerRef, r.PublicationSHA256); err != nil {
		return err
	}
	switch r.Code {
	case ownerOK, ownerPending, ownerBusy, ownerIncompatible, ownerRefused, ownerFault, ownerTimeout:
	default:
		return ErrIdentity
	}
	if r.Startup != (StartupReference{}) {
		if err := r.Startup.Validate(); err != nil {
			return err
		}
	}
	if err := r.validatePreparationSubject(); err != nil {
		return err
	}
	if err := r.validatePreparationResult(); err != nil {
		return err
	}
	return r.validateActionSubject()
}

func (r ownerReply) validatePreparationResult() error {
	if err := r.Failure.Validate(); err != nil {
		return err
	}
	if err := r.Report.ValidateCondition(PreparationCondition(r.Code)); err != nil {
		return err
	}
	if r.Action == ownerMigrateOwn && r.Code == ownerOK && r.Report == (ManualMigrationReport{}) {
		return ErrIdentity
	}
	if r.Action != ownerMigrateOwn && r.Report != (ManualMigrationReport{}) {
		return ErrIdentity
	}
	if (!ownPreparationAction(r.Action) || r.Code == ownerOK) && r.Failure != (PreparationFailure{}) {
		return ErrIdentity
	}
	return nil
}

func (r ownerReply) validatePreparationSubject() error {
	// Own replies name the canonical own target, while request subjects omit it.
	subjectTarget := r.Target
	if ownPreparationAction(r.Action) {
		subjectTarget = originRecord{}
	}
	subject := ownerRequest{Owner: r.OwnerRef, Action: r.Action, RootID: r.RootID, Target: subjectTarget, Context: r.Context, Source: r.Source, Initial: r.Initial}
	if ownPreparationAction(r.Action) {
		subject.InitialResultFor = r.InitialResultFor
	}
	if err := subject.validateOperationSubject(); err != nil {
		return err
	}
	if r.Disposition != (Disposition{}) {
		if r.Action == ownerStatus || r.Action == ownerEnsureOther {
			return ErrIdentity
		}
		return r.Disposition.Validate()
	}
	if r.Code == ownerOK && ownPreparationAction(r.Action) && (r.Action != ownerMigrateOwn || r.Report == (ManualMigrationReport{})) {
		return ErrIdentity
	}
	return nil
}

func (r ownerReply) validateActionSubject() error {
	accepted := r.Code == ownerOK || r.Code == ownerPending
	switch r.Action {
	case ownerStatus:
		if r.Target != (originRecord{}) || r.Startup != (StartupReference{}) || r.InitialResultFor != (StartupReference{}) {
			return ErrIdentity
		}
	case ownerEnsureOwn, ownerRecoverSaved, ownerPrepareContinuation, ownerMigrateOwn:
		return r.validateOwnSubject(accepted)
	case ownerEnsureOther:
		if r.InitialResultFor != (StartupReference{}) && r.InitialResultFor != r.Startup {
			return ErrIdentity
		}
		if err := validateOwnerTarget(r.Target, r.RootID); err != nil {
			return err
		}
		if accepted {
			return r.Startup.Validate()
		}
	default:
		return ErrIdentity
	}
	return nil
}

func (r ownerReply) validateOwnSubject(accepted bool) error {
	if r.Target == (originRecord{}) && !accepted && r.Startup == (StartupReference{}) {
		return nil
	}
	if _, err := NewOrigin(r.Target.Root, r.Target.RootID); err != nil {
		return err
	}
	if r.Target.RootID != r.RootID {
		return ErrIdentity
	}
	if accepted && r.Startup != r.OwnerRef {
		return ErrIdentity
	}
	if r.Startup != (StartupReference{}) && r.Startup != r.OwnerRef {
		return ErrIdentity
	}
	return nil
}

func validateOwnerSubject(protocol int, rootID string, owner StartupReference, publication string) error {
	if protocol != ownerProtocolVersion {
		return errors.Join(ErrIdentity, ErrOwnerIncompatible)
	}
	return errors.Join(task.ValidateRootID(rootID), owner.Validate(), task.ValidateSHA256(publication))
}

func validateOwnerTarget(target originRecord, ownerRootID string) error {
	if _, err := NewOrigin(target.Root, target.RootID); err != nil {
		return err
	}
	if target.RootID == ownerRootID {
		return ErrIdentity
	}
	return nil
}

type ownerWireFrame interface {
	ownerRequest | ownerReply | ownerStopRequest | ownerStopReply
	validate() error
}

// ownerFrameError preserves error identity without rendering untrusted JSON or
// domain-validation diagnostics. Wire input is not sealed task evidence.
type ownerFrameError struct{ cause error }

func (ownerFrameError) Error() string   { return "infrastructure owner control frame failed" }
func (e ownerFrameError) Unwrap() error { return e.cause }

func invalidOwnerFrame(cause error) error {
	return ownerFrameError{errors.Join(ErrIdentity, cause)}
}

// readOwnerFrame consumes one newline frame. Later frames remain buffered for
// an explicit later read; connection lifetime and I/O deadlines belong to callers.
// Any frame error requires connection closure. Transport faults retain their
// I/O classification; only invalid bytes or domain values claim ErrIdentity.
func readOwnerFrame[T ownerWireFrame](input *bufio.Reader) (T, error) {
	data, err := readOwnerFrameBytes(input)
	if err != nil {
		var zero T
		return zero, err
	}
	return decodeOwnerFrame[T](data)
}

func readOwnerFrameBytes(input *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		part, err := input.ReadSlice('\n')
		if len(part) > task.MaxControlRecordSize-len(data) {
			return nil, invalidOwnerFrame(task.ErrControlRecordTooBig)
		}
		data = append(data, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if len(data) > 0 {
				err = errors.Join(io.ErrUnexpectedEOF, err)
			}
			return nil, ownerFrameError{err}
		}
		return data, nil
	}
}

func decodeOwnerFrame[T ownerWireFrame](data []byte) (T, error) {
	var zero T
	var envelope struct {
		Protocol     int `json:"protocol"`
		StopProtocol int `json:"stop_protocol"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return zero, invalidOwnerFrame(err)
	}
	if envelope.StopProtocol != 0 {
		if envelope.StopProtocol != ownerStopProtocolVersion {
			return zero, invalidOwnerFrame(ErrOwnerIncompatible)
		}
	} else if envelope.Protocol != ownerProtocolVersion {
		return zero, invalidOwnerFrame(ErrOwnerIncompatible)
	}
	var result T
	if err := task.DecodeStrict(data, &result); err != nil {
		return zero, invalidOwnerFrame(err)
	}
	if err := result.validate(); err != nil {
		return zero, invalidOwnerFrame(err)
	}
	return result, nil
}

func writeOwnerFrame[T ownerWireFrame](writer io.Writer, frame T) error {
	if err := frame.validate(); err != nil {
		return invalidOwnerFrame(err)
	}
	data, err := task.MarshalCanonical(frame)
	if err != nil {
		return invalidOwnerFrame(err)
	}
	if len(data) > task.MaxControlRecordSize {
		return invalidOwnerFrame(task.ErrControlRecordTooBig)
	}
	n, err := writer.Write(data)
	if err != nil {
		return ownerFrameError{err}
	}
	if n != len(data) {
		return ownerFrameError{io.ErrShortWrite}
	}
	return nil
}
