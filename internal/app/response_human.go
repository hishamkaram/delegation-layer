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
	writeHumanSummary(&output, response)
	writeHumanSupervisor(&output, response.SupervisorControl)
	writeHumanContinuation(&output, response.Continuation)
	writeHumanPayload(&output, response.Payload)
	writeHumanOutputDiagnostics(&output, response.Diagnostics)
	writeHumanRecheck(&output, response.Recheck)
	writeHumanRaw(&output, response.Raw)
	writeHumanStops(&output, response.Stop, response.Stops)
	writeHumanWorkspaceResponse(&output, response)
	if err := writeHumanInteraction(&output, response); err != nil {
		return err
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
	if control.MigrationPhase != "" {
		fmt.Fprintf(output, " migration_phase=%s", control.MigrationPhase)
	}
	output.WriteByte('\n')
}

func writeHumanContinuation(output *strings.Builder, continuation *ContinuationResponse) {
	if continuation == nil {
		return
	}
	fmt.Fprintf(output, "continuation resumable=%t mode=%s", continuation.Resumable, continuation.Mode)
	if continuation.PredecessorID != "" {
		fmt.Fprintf(output, " predecessor_task_id=%s", continuation.PredecessorID)
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
