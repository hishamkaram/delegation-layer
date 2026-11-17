package app

import (
	"fmt"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func writeHumanResponse(w io.Writer, response Response) error {
	var output strings.Builder
	if response.Command != "schedule" {
		writeHumanSummary(&output, response)
	}
	if saved := response.Saved; saved != nil {
		fmt.Fprintf(&output, "saved path=%q length=%d sha256=%s\n", saved.Path, saved.Length, saved.SHA256)
	}
	writeHumanWait(&output, response.Wait)
	writeHumanMaintenanceFacts(&output, response.ControlFact, response.Settlement)
	if settled := response.Settlement; settled != nil {
		fmt.Fprintf(&output, "settlement administrative_state=%s native_execution=%s permission=%s challenge=%s confirmed_at=%s held_claims=%s\n", settled.AdministrativeState, settled.NativeExecution, settled.Permission, settled.Challenge, settled.ConfirmedAt, strings.Join(settled.HeldClaims, ","))
	}
	writeHumanScheduleReliability(&output, response)
	if response.LastProviderOutputAt != "" {
		fmt.Fprintf(&output, "last_provider_output_at=%s source=capture_metadata\n", response.LastProviderOutputAt)
	}
	writeHumanResourceAdmission(&output, response.ResourceAdmission)
	if selected := response.Team; selected != nil {
		fmt.Fprintf(&output, "team scope=%s id=%s member=%s revision=%d\n", selected.Target.Team.Scope, selected.Target.Team.TeamID, selected.Target.MemberID, selected.TeamRevision)
	}
	writeHumanSupervisor(&output, response.SupervisorControl)
	writeHumanSupervisorOwner(&output, response.SupervisorOwner)
	writeHumanContinuation(&output, response.Continuation)
	writeHumanPayload(&output, response.Payload)
	writeHumanOutputDiagnostics(&output, response.Diagnostics)
	writeHumanRecheck(&output, response.Recheck)
	writeHumanRaw(&output, response.Raw)
	writeHumanStops(&output, response.Stop, response.Stops)
	writeHumanWorkspaceResponse(&output, response)
	writeHumanCheckpointResponse(&output, response)
	if err := writeHumanScheduleResponse(&output, response); err != nil {
		return err
	}
	if err := writeHumanInteraction(&output, response); err != nil {
		return err
	}
	if response.Command == "schedule" && response.Schedule != nil && response.Schedule.Request != nil && output.Len() > 2<<20 {
		return ErrJSONResponseTooLarge
	}
	if (response.ScheduleHistory != nil || response.ScheduleExecution != nil) && output.Len() > MaxJSONResponseBytes {
		return ErrJSONResponseTooLarge
	}
	return writeAll(w, []byte(output.String()))
}

func writeHumanSummary(output *strings.Builder, response Response) {
	fmt.Fprintf(output, "command=%s root_id=%s task_id=%s admission=%s liveness=%s publication=%s", response.Command, response.RootID, response.TaskID, response.Admission, response.Liveness, response.Publication)
	if response.Status != "" {
		fmt.Fprintf(output, " status=%s", response.Status)
	}
	output.WriteByte('\n')
}

func writeHumanSupervisor(output *strings.Builder, control *SupervisorControlResponse) {
	if control == nil {
		return
	}
	fmt.Fprintf(output, "supervisor action=%s state=%s legacy_reachability=%s active_work=%s next_action=%s", control.Action, control.State, control.LegacyReachability, control.ActiveWork, control.NextAction)
	if control.MigrationReadiness != "" {
		fmt.Fprintf(output, " migration_readiness=%s", control.MigrationReadiness)
	}
	if control.ObservedAt != "" {
		fmt.Fprintf(output, " observed_at=%s", control.ObservedAt)
	}
	if control.DiagnosticSnapshotAt != "" {
		fmt.Fprintf(output, " diagnostic_snapshot_at=%s", control.DiagnosticSnapshotAt)
	}
	if control.MigrationPhase != "" {
		fmt.Fprintf(output, " migration_phase=%s", control.MigrationPhase)
	}
	output.WriteByte('\n')
	if binding := control.InstallationBinding; binding != nil {
		fmt.Fprintf(output, "installation_binding scope=%s verification=%s relation=%s client_selected=%s client_recorded=%s daemon_selected=%s daemon_recorded=%s\n", binding.Scope, binding.Verification, binding.Relation, binding.Client.SelectedAvailability, binding.Client.RecordedAvailability, binding.Daemon.SelectedAvailability, binding.Daemon.RecordedAvailability)
	}
	if blocker := control.FirstBlocker; blocker != nil {
		fmt.Fprintf(output, "first_blocker kind=%s task_id=%s operation_id=%s queue_state=%s reason_code=%s next_action=%s\n", blocker.Kind, blocker.TaskID, blocker.OperationID, blocker.QueueState, blocker.ReasonCode, blocker.NextAction)
	}
	if upgrade := control.Upgrade; upgrade != nil {
		fmt.Fprintf(output, "upgrade operation_id=%s mode=%s phase=%s completed=%t\n", upgrade.OperationID, upgrade.Mode, upgrade.Phase, upgrade.Completed)
		if len(upgrade.CompletionCommand) > 0 {
			fmt.Fprintf(output, "completion_command=%q\n", upgrade.CompletionCommand)
		}
	}
	if restart := control.RuntimeRestart; restart != nil {
		fmt.Fprintf(output, "runtime_restart from_startup=%s", restart.FromStartup)
		if restart.Phase != "" {
			fmt.Fprintf(output, " phase=%s", restart.Phase)
		}
		if restart.SuccessorStartup != "" {
			fmt.Fprintf(output, " successor_startup=%s", restart.SuccessorStartup)
		}
		if restart.Custody != "" {
			fmt.Fprintf(output, " custody=%s observed_at=%s", restart.Custody, restart.ObservedAt)
		}
		if restart.OperationPending {
			output.WriteString(" operation_pending=true")
		}
		output.WriteByte('\n')
	}
}

func writeHumanContinuation(output *strings.Builder, continuation *ContinuationResponse) {
	if continuation == nil {
		return
	}
	fmt.Fprintf(output, "continuation resumable=%t mode=%s", continuation.Resumable, continuation.Mode)
	if continuation.PredecessorID != "" {
		fmt.Fprintf(output, " predecessor_task_id=%s", continuation.PredecessorID)
	}
	if continuation.SessionSource != "" {
		fmt.Fprintf(output, " session_source=%s", continuation.SessionSource)
	}
	if continuation.ContinueCommand != "" {
		fmt.Fprintf(output, " command=%s", continuation.ContinueCommand)
	}
	output.WriteByte('\n')
}

func writeHumanPayload(output *strings.Builder, payload *task.PayloadDescriptor) {
	if payload != nil {
		fmt.Fprintf(output, "payload=%s length=%d sha256=%s\n", payload.Basename, payload.Length, payload.SHA256)
	}
}

func writeHumanRecheck(output *strings.Builder, recheck *RecheckResponse) {
	if recheck == nil {
		return
	}
	fmt.Fprintf(output, "recheck status=%s assessment_id=%s report=%s", recheck.Status, recheck.AssessmentID, recheck.ReportPath)
	if recheck.AnswerPath != "" {
		fmt.Fprintf(output, " answer=%s", recheck.AnswerPath)
	}
	output.WriteByte('\n')
}

func writeHumanRaw(output *strings.Builder, raw []taskdir.LogDescriptor) {
	for _, descriptor := range raw {
		fmt.Fprintf(output, "raw path=%s available=%t sealed=%t", descriptor.Path, descriptor.Available, descriptor.Sealed)
		if descriptor.Size != nil {
			fmt.Fprintf(output, " size=%d", *descriptor.Size)
		}
		if descriptor.SHA256 != "" {
			fmt.Fprintf(output, " sha256=%s", descriptor.SHA256)
		}
		output.WriteByte('\n')
	}
}

func writeHumanStops(output *strings.Builder, stop *StopResponse, stops []StopResponse) {
	if stop != nil {
		writeHumanStop(output, "stop", *stop)
	}
	for _, item := range stops {
		writeHumanStop(output, "stop", item)
	}
}

func writeHumanOutputDiagnostics(output *strings.Builder, diagnostics []task.OutputDiagnostic) {
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(output, "diagnostic code=%s message=%s", diagnostic.Code, diagnostic.Message)
		if diagnostic.Field != "" {
			fmt.Fprintf(output, " field=%s", diagnostic.Field)
		}
		if diagnostic.Event != 0 {
			fmt.Fprintf(output, " event=%d", diagnostic.Event)
		}
		if diagnostic.Offset != 0 {
			fmt.Fprintf(output, " offset=%d", diagnostic.Offset)
		}
		output.WriteByte('\n')
	}
}

func writeHumanStop(output *strings.Builder, prefix string, stop StopResponse) {
	fmt.Fprintf(output, "%s request_id=%s requested=%t matched=%t attempted=%t in_flight=%t observed_state=%s terminated=%t", prefix, stop.RequestID, stop.Requested, stop.Matched, stop.Attempted, stop.InFlight, stop.ObservedState, stop.Terminated)
	if stop.Action != "" {
		fmt.Fprintf(output, " action=%s", stop.Action)
	}
	if stop.NumericTaskID != nil {
		fmt.Fprintf(output, " numeric_task_id=%d", *stop.NumericTaskID)
	}
	if stop.Acknowledged != nil {
		fmt.Fprintf(output, " acknowledged=%t", *stop.Acknowledged)
	}
	output.WriteByte('\n')
}

func writeHumanMaintenanceFacts(output *strings.Builder, controlFact *ControlFactResponse, settlement *SettlementResponse) {
	if fact := controlFact; fact != nil {
		fmt.Fprintf(output, "control_fact kind=%s id=%s fact=%s name=%q reason_code=%s next_action=%s\n", fact.ResourceKind, fact.ResourceID, fact.FactKind, fact.Name, fact.ReasonCode, fact.NextAction)
	}
	if settled := settlement; settled != nil && settled.TargetKind != "" {
		fmt.Fprintf(output, "settlement_target kind=%s operation_id=%s acknowledged_missing_facts=%s\n", settled.TargetKind, settled.OperationID, strings.Join(settled.AcknowledgedMissingFacts, ","))
	}
}
