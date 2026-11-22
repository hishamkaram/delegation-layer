package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const ownerUpgradeProtocolVersion = 5

// V5 has distinct DTOs. Strict decoding forbids every legacy operation subject,
// including initial_result_for, even when its supplied value would be zero.
type ownerUpgradeRequest struct {
	Protocol          int              `json:"protocol"`
	RootID            string           `json:"root_id"`
	Owner             StartupReference `json:"owner"`
	PublicationSHA256 string           `json:"publication_sha256"`
	Client            ProcessIdentity  `json:"client"`
	Deadline          string           `json:"deadline"`
	Request           UpgradeRequest   `json:"request"`
}

type ownerUpgradeReply struct {
	Protocol          int              `json:"protocol"`
	RootID            string           `json:"root_id"`
	Owner             StartupReference `json:"owner"`
	PublicationSHA256 string           `json:"publication_sha256"`
	Client            ProcessIdentity  `json:"client"`
	Deadline          string           `json:"deadline"`
	Request           UpgradeRequest   `json:"request"`
	Outcome           UpgradeOutcome   `json:"outcome"`
}

func (r ownerUpgradeRequest) validate() error {
	if r.Protocol != ownerUpgradeProtocolVersion {
		return ErrOwnerIncompatible
	}
	return errors.Join(r.Request.Validate(), r.authenticationRequest().validate())
}

// The projection reuses native/record authentication only, exactly as stop1
// does. V5 action, OP and context are independently validated at both edges.
func (r ownerUpgradeRequest) authenticationRequest() ownerRequest {
	return ownerRequest{
		Protocol: ownerProtocolVersion, RootID: r.RootID, Owner: r.Owner,
		PublicationSHA256: r.PublicationSHA256, Client: r.Client, Deadline: r.Deadline, Action: ownerStatus,
	}
}

func (r ownerUpgradeReply) subject() ownerUpgradeRequest {
	return ownerUpgradeRequest{
		Protocol: r.Protocol, RootID: r.RootID, Owner: r.Owner,
		PublicationSHA256: r.PublicationSHA256, Client: r.Client, Deadline: r.Deadline, Request: r.Request,
	}
}

func (r ownerUpgradeReply) validate() error {
	if err := errors.Join(r.subject().validate(), validateUpgradeOutcomeSubject(r.Request, r.Outcome)); err != nil {
		return err
	}
	if r.Outcome == (UpgradeOutcome{}) {
		return ErrIdentity
	}
	if r.Request.Action == UpgradeStatus && (r.Outcome.Report != (UpgradeReport{}) || r.Outcome.Disposition != (Disposition{}) || r.Outcome.FailureFact != (UpgradeFailureFact{})) {
		return ErrIdentity
	}
	return nil
}

func upgradeReply(request ownerUpgradeRequest, outcome UpgradeOutcome) ownerUpgradeReply {
	return ownerUpgradeReply{
		Protocol: request.Protocol, RootID: request.RootID, Owner: request.Owner,
		PublicationSHA256: request.PublicationSHA256, Client: request.Client, Deadline: request.Deadline,
		Request: request.Request, Outcome: outcome,
	}
}

func (s *ownerServer) prepareUpgradePreflight(ctx context.Context) (resultErr error) {
	defer func() { s.completeUpgradePreflight(resultErr) }()
	if ctx == nil {
		return os.ErrInvalid
	}
	deadline, err := canonicalStartupTime(s.entry.operation.Intent().Deadline)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if _, _, _, _, err = s.readResident(bounded); err != nil {
		return err
	}
	_, _, err = resolveSelectedContext(bounded, s.journal, s.entry.operation.defaultContextReference(), LaunchContextReference{})
	return err
}

// The initial completion mutex/channel is shared, but its result domains are
// disjoint. Preflight never creates a legacy outcome or terminal handoff.
func (s *ownerServer) completeUpgradePreflight(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upgradePreflight = UpgradePreflight{State: UpgradePreflightPassed}
	if err != nil {
		s.upgradePreflight = UpgradePreflight{State: UpgradePreflightFailed, ReasonCode: "upgrade_preflight_failed"}
	}
	s.preflightError = err
	s.initialCompleted = true
	close(s.initialDone)
}

func (s *ownerServer) observeUpgradePreflight() UpgradePreflight {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initialCompleted {
		return UpgradePreflight{State: UpgradePreflightPending}
	}
	if s.entry.operation.Intent().Inputs.Initial.Kind == InitialUpgradePreflight {
		return s.upgradePreflight
	}
	o := s.initialOutcome
	if s.initialError == nil && o != (PreparationOutcome{}) && o.Condition == PreparationOK && o.Validate() == nil &&
		(o.Disposition != (Disposition{}) || o.Report != (ManualMigrationReport{}) || o.RuntimeRestart != (RuntimeRestartReport{})) {
		return UpgradePreflight{State: UpgradePreflightPassed}
	}
	return UpgradePreflight{State: UpgradePreflightFailed, ReasonCode: "initial_preparation_failed"}
}

func (s *ownerServer) handleUpgradeConnection(parent context.Context, connection *net.UnixConn, request ownerUpgradeRequest) {
	ctx, cancel, err := ownerContactContext(parent, request.authenticationRequest())
	if err != nil {
		s.connectionFault(err)
		return
	}
	defer cancel()
	deadline, _ := ctx.Deadline() // The shared context constructor requires one.
	if err = s.setRequestDeadline(connection, deadline); err != nil {
		s.connectionFault(err)
		return
	}
	intent, assumption, publication, operation, err := s.readResident(ctx)
	if err == nil {
		err = authenticateOwnerRequest(ctx, connection, intent, assumption, publication, request.authenticationRequest())
	}
	if err != nil {
		s.connectionFault(err)
		return
	}
	selected, _, err := resolveSelectedContext(ctx, s.journal, operation.defaultContextReference(), request.Request.Context)
	if err != nil {
		s.writeUpgradeReply(connection, request, UpgradeOutcome{Condition: UpgradeRefused, Preflight: s.observeUpgradePreflight(), ReasonCode: "upgrade_context_invalid"})
		return
	}
	action := ownerEnsureOwn
	if request.Request.Action == UpgradeStatus {
		action = ownerStatus
	}
	if err = s.admitConnection(connection, action); err != nil {
		s.writeUpgradeReply(connection, request, UpgradeOutcome{Condition: UpgradeBusy, Preflight: s.observeUpgradePreflight(), ReasonCode: "owner_busy"})
		return
	}
	outcome := s.executeUpgrade(ctx, operation, selected, request.Request)
	s.writeUpgradeReply(connection, request, outcome)
}

func (s *ownerServer) writeUpgradeReply(connection *net.UnixConn, request ownerUpgradeRequest, outcome UpgradeOutcome) {
	if err := writeOwnerFrame(connection, upgradeReply(request, outcome)); err != nil {
		s.connectionFault(err)
	}
}

func (s *ownerServer) executeUpgrade(ctx context.Context, operation Operation, selected LaunchContext, request UpgradeRequest) UpgradeOutcome {
	preflight := s.observeUpgradePreflight()
	outcome := UpgradeOutcome{Condition: UpgradeOK, Preflight: preflight}
	switch preflight.State {
	case UpgradePreflightPending:
		outcome.Condition = UpgradePending
	case UpgradePreflightFailed:
		outcome.Condition, outcome.ReasonCode = UpgradeRefused, preflight.ReasonCode
	case UpgradePreflightPassed:
	}
	if outcome.Condition != UpgradeOK {
		return outcome
	}
	host, supported := s.host.(UpgradeHost)
	if !supported {
		outcome.Condition, outcome.ReasonCode = UpgradeUnsupported, "owner_upgrade_unsupported"
		return outcome
	}
	if request.Action == UpgradeStatus {
		return outcome
	}
	s.mu.Lock()
	reconciling := s.runtimeFrom != (StartupReference{}) || s.terminalHandoff
	s.mu.Unlock()
	if reconciling {
		outcome.Condition, outcome.ReasonCode = UpgradePending, "owner_reconciliation_pending"
		return outcome
	}
	result, err := host.UpgradeOwnRoot(ctx, operation, selected.Clone(), request, s.entry.grant, s.waits)
	if result.Preflight == (UpgradePreflight{}) {
		result.Preflight = preflight
	}
	if err != nil {
		result.Condition = UpgradeFault
		result.FailureFact = upgradeFailureFact(err)
	}
	if invalid := validateUpgradeHostOutcome(result, preflight, request); invalid != nil {
		s.connectionFault(errors.Join(err, invalid))
		outcome.Condition, outcome.ReasonCode = UpgradeFault, "upgrade_host_outcome_invalid"
		return outcome
	}
	if err != nil {
		s.connectionFault(err)
	}
	return result
}

func upgradeFailureFact(cause error) UpgradeFailureFact {
	var fact *task.ControlFactError
	if !errors.As(cause, &fact) || fact == nil {
		return UpgradeFailureFact{}
	}
	return UpgradeFailureFact{ResourceKind: fact.ResourceKind, ResourceID: fact.ResourceID, FactKind: fact.FactKind, Name: fact.Name, ReasonCode: fact.ReasonCode, NextAction: fact.NextAction}
}

func validateUpgradeHostOutcome(outcome UpgradeOutcome, preflight UpgradePreflight, request UpgradeRequest) error {
	if outcome == (UpgradeOutcome{}) || outcome.Preflight != preflight {
		return ErrIdentity
	}
	return validateUpgradeOutcomeSubject(request, outcome)
}

func validateUpgradeOutcomeSubject(request UpgradeRequest, outcome UpgradeOutcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	if request.OperationID != "" && outcome.Report != (UpgradeReport{}) && request.OperationID != outcome.Report.OperationID {
		return ErrIdentity
	}
	if request.Action == UpgradeReadyCurrent && !readyCurrentReportValid(outcome) {
		return ErrIdentity
	}
	if outcome.Disposition.Kind == DispositionStarted && outcome.Disposition.Context != request.Context {
		return ErrIdentity
	}
	if request.Action != UpgradeStatus && outcome.Condition == UpgradeOK && outcome.Report == (UpgradeReport{}) && outcome.Disposition == (Disposition{}) {
		return ErrIdentity
	}
	return nil
}

func readyCurrentReportValid(outcome UpgradeOutcome) bool {
	if outcome.Report == (UpgradeReport{}) {
		return outcome.Condition != UpgradeOK
	}
	return outcome.Report.Completed
}

// ProbeBoundUpgrade is a passive authenticated V5 handshake at exactly expected.
// It neither refreshes an owner nor interprets transport failure as old support.
func ProbeBoundUpgrade(ctx context.Context, store *taskdir.ReadOnlyStore, expected StartupReference) (UpgradePreflight, error) {
	request, pin, err := boundUpgradeRequest(ctx, store, expected, UpgradeRequest{Action: UpgradeStatus})
	if err != nil {
		return UpgradePreflight{}, err
	}
	reply, err := contactUpgradeOwner(ctx, store, pin, request)
	if reply == (ownerUpgradeReply{}) {
		return UpgradePreflight{}, err
	}
	return reply.Outcome.Preflight, errors.Join(err, upgradeOutcomeCause(reply.Outcome.Condition))
}

// RequestBoundUpgrade authenticates passed V5 status before one mutation. Neither
// an uncertain write nor a failed exchange is retried, refreshed or downgraded.
func RequestBoundUpgrade(ctx context.Context, store *taskdir.ReadOnlyStore, expected StartupReference, selection UpgradeRequest) (UpgradeOutcome, error) {
	if selection.Action == UpgradeStatus {
		return UpgradeOutcome{}, ErrIdentity
	}
	request, pin, err := boundUpgradeRequest(ctx, store, expected, selection)
	if err != nil {
		return UpgradeOutcome{}, err
	}
	status := request
	status.Request = UpgradeRequest{Action: UpgradeStatus, Context: request.Request.Context}
	handshake, err := contactUpgradeOwner(ctx, store, pin, status)
	if err != nil || handshake == (ownerUpgradeReply{}) {
		return UpgradeOutcome{}, err
	}
	if handshake.Outcome.Condition != UpgradeOK || handshake.Outcome.Preflight.State != UpgradePreflightPassed {
		return handshake.Outcome, upgradeOutcomeCause(handshake.Outcome.Condition)
	}
	reply, err := contactUpgradeOwner(ctx, store, pin, request)
	if reply == (ownerUpgradeReply{}) {
		return UpgradeOutcome{}, err
	}
	return reply.Outcome, err
}

func boundUpgradeRequest(ctx context.Context, store *taskdir.ReadOnlyStore, expected StartupReference, selection UpgradeRequest) (ownerUpgradeRequest, taskdir.RootPin, error) {
	if err := expected.Validate(); err != nil {
		return ownerUpgradeRequest{}, taskdir.RootPin{}, err
	}
	legacy, pin, err := currentOwnerRequestAtStartup(ctx, store, ownerStatus, expected)
	if err != nil {
		return ownerUpgradeRequest{}, pin, err
	}
	if selection.Context == (LaunchContextReference{}) {
		intent, readErr := readOwnerContactIntent(ctx, store, pin, expected)
		if readErr != nil {
			return ownerUpgradeRequest{}, pin, readErr
		}
		selection.Context = intent.Inputs.Context
	}
	request := ownerUpgradeRequest{
		Protocol: ownerUpgradeProtocolVersion, RootID: legacy.RootID, Owner: legacy.Owner,
		PublicationSHA256: legacy.PublicationSHA256, Client: legacy.Client, Deadline: legacy.Deadline, Request: selection,
	}
	return request, pin, request.validate()
}

func upgradeOutcomeCause(condition UpgradeCondition) error {
	switch condition {
	case UpgradeOK:
		return nil
	case UpgradePending:
		return ErrOwnerPending
	case UpgradeBusy:
		return ErrOwnerBusy
	case UpgradeUnsupported:
		return ErrOwnerIncompatible
	case UpgradeTimeout:
		return errors.Join(ErrOwnerRequest, context.DeadlineExceeded)
	case UpgradeRefused, UpgradeFault:
		return ErrOwnerRequest
	}
	return ErrIdentity
}

func contactUpgradeOwner(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, request ownerUpgradeRequest) (reply ownerUpgradeReply, resultErr error) {
	if err := request.validate(); err != nil {
		return reply, err
	}
	operation, cancel, err := ownerContactContext(ctx, request.authenticationRequest())
	if err != nil {
		return reply, err
	}
	defer cancel()
	intent, assumption, publication, err := readOwnerContactRecords(operation, store, pin, request.authenticationRequest())
	if err != nil {
		return reply, err
	}
	if err = validateUpgradeContactClient(request.Client); err != nil {
		return reply, err
	}
	connection, err := dialOwnerContact(operation, publication.Endpoint.Path)
	if err != nil {
		return reply, err
	}
	verified := false
	finish := watchOwnerContactCancellation(operation, connection)
	defer func() {
		cleanupErr := errors.Join(finish(), connection.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, ownerContactContextError(operation))
		}
		resultErr = errors.Join(resultErr, cleanupErr)
		if resultErr != nil {
			if !verified {
				reply = ownerUpgradeReply{}
			}
			resultErr = ownerContactError{cause: resultErr}
		}
	}()
	if err = authenticateOwnerPeer(operation, connection, intent, assumption, publication); err != nil {
		return reply, err
	}
	if err = checkOwnerContactEndpoint(operation, store, pin, publication.Endpoint); err != nil {
		return reply, err
	}
	if err = writeOwnerFrame(connection, request); err != nil {
		return reply, err
	}
	reply, err = readOwnerFrame[ownerUpgradeReply](bufio.NewReader(connection))
	if err != nil {
		return ownerUpgradeReply{}, err
	}
	if reply.subject() != request {
		return ownerUpgradeReply{}, ErrIdentity
	}
	if _, _, _, err = readOwnerContactRecords(operation, store, pin, request.authenticationRequest()); err != nil {
		return ownerUpgradeReply{}, err
	}
	verified = true
	return reply, nil
}

func validateUpgradeContactClient(client ProcessIdentity) error {
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return err
	}
	if client != (ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()}) {
		return ErrIdentity
	}
	return nil
}
