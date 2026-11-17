package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestControlFactProjectionPreservesCauseWithoutPublishingIt(t *testing.T) {
	cause := errors.New("private inherited content")
	err := &task.ControlFactError{ResourceKind: "inspection", ResourceID: waitFirstID, FactKind: "evidence_record", Name: "completion.json", ReasonCode: "evidence_missing", NextAction: "inspect_evidence", Cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("boundary lost original cause")
	}
	response := newResponse("settle")
	var output bytes.Buffer
	if writeErr := writeJSONResponse(&output, "settle", &response, err); writeErr != nil {
		t.Fatal(writeErr)
	}
	if bytes.Contains(output.Bytes(), []byte(cause.Error())) {
		t.Fatal("private native cause escaped projection")
	}
	var got Response
	if decodeErr := json.Unmarshal(output.Bytes(), &got); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if got.ControlFact == nil || got.ControlFact.Name != "completion.json" || got.ControlFact.ResourceKind != "inspection" {
		t.Fatalf("missing typed fact: %+v", got.ControlFact)
	}
}

func TestInspectionStatusGuidanceStaysObservational(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	id := strings.Repeat("f", 32)
	op, err := store.OpenInspectionContext(t.Context(), id, true)
	requireSettlementTest(t, err)
	requireSettlementTest(t, op.Close())
	result := statusContext(t.Context(), Arguments{Root: store.Root, TaskID: id}, Dependencies{})
	var fact *task.ControlFactError
	if result.code != 1 || !errors.As(result.err, &fact) || fact.ResourceKind != "inspection" || fact.ReasonCode != "inspection_not_task" || fact.NextAction != "inspect_inspection" || result.response.Settlement != nil {
		t.Fatalf("status suggested maintenance mutation: %+v error=%v", result.response, result.err)
	}
}

func TestUpgradeOwnerFailureFactReachesPublicDiagnostic(t *testing.T) {
	outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeFault, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, FailureFact: infrastructure.UpgradeFailureFact{
		ResourceKind: "inspection", ResourceID: waitFirstID, FactKind: "evidence_record", Name: ".operation.lock", ReasonCode: "evidence_unreadable", NextAction: "inspect_evidence",
	}}
	result := finishSupervisorUpgrade(Arguments{}, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "supervisor", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if result.code != 1 || response.ControlFact == nil || response.ControlFact.Name != ".operation.lock" || response.ControlFact.ResourceID != waitFirstID || response.SupervisorControl.NextAction != "stop_and_report" {
		t.Fatalf("owner diagnostic was lost: %s", output.Bytes())
	}
}

func TestSettlementAdmissionOnlyNamesReceiptAndCancellation(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	_, meta, err := td.PreparedRecords()
	requireSettlementTest(t, err)
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	requireSettlementTest(t, err)
	requireSettlementTest(t, errors.Join(permit.Consume(), permit.Release()))
	_, err = observeSettlementQueue(t.Context(), store, td, supervisorControlOptions(Dependencies{}), Dependencies{})
	var fact *task.ControlFactError
	if !errors.As(err, &fact) || fact.Name != "supervisor.ref.json" || fact.NextAction != "cancel_task" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cancellation fact: %v", err)
	}
}

func TestSettlementInspectionKindIsExplicit(t *testing.T) {
	args, err := ParseArguments([]string{"settle", waitFirstID, "--kind", "inspection", "--prepare", "--json"})
	if err != nil || args.SettlementKind != "inspection" {
		t.Fatalf("inspection route: %+v %v", args, err)
	}
	if _, err = ParseArguments([]string{"settle", waitFirstID, "--kind", "unknown", "--prepare"}); err == nil {
		t.Fatal("unknown target kind accepted")
	}
}

func requireNamedStatusFact(t *testing.T, response Response, expected ControlFactResponse) {
	t.Helper()
	if response.ControlFact == nil || *response.ControlFact != expected {
		t.Fatalf("status lost its named control fact: got=%+v want=%+v", response.ControlFact, expected)
	}
}
