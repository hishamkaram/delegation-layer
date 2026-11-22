package app

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestColdUpgradeFailureDiagnosticPreservesSafeFacts(t *testing.T) {
	const private = "PRIVATE-COLD-DIAGNOSTIC-CANARY"
	const operation = "upgrade-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fact := ControlFactResponse{ResourceKind: "inspection", ResourceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FactKind: "evidence_file", Name: ".operation.lock", ReasonCode: "evidence_unreadable", NextAction: "inspect_evidence"}
	response := Response{
		Error:       private,
		ControlFact: &fact,
		SupervisorControl: &SupervisorControlResponse{
			State: "blocked", MigrationPhase: "migrating", ReasonCode: "", NextAction: "stop_and_report",
			Upgrade:        &SupervisorUpgradeResponse{OperationID: operation, Phase: "migrating", CompletionCommand: []string{private}},
			RuntimeRestart: &SupervisorRuntimeRestartResponse{FromStartup: private, SuccessorStartup: private},
		},
	}
	got := coldUpgradeFailureFacts(response, []string{"ensure-owner", "--complete-upgrade", operation, private}, pueue.PrivateMigrationPredecessorStopped, true, nil)
	want := coldUpgradeDiagnostic{Step: "complete_upgrade", ControlPresent: true, State: "blocked", MigrationPhase: "migrating", UpgradePhase: "migrating", ReasonCode: "", NextAction: "stop_and_report", OperationID: operation, ControlFact: &fact, OwnerMigrationState: "observed", OwnerMigrationPhase: pueue.PrivateMigrationPredecessorStopped}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("safe facts changed: got=%+v want=%+v", got, want)
	}
	data, err := json.Marshal(got)
	if err != nil || strings.Contains(string(data), private) {
		t.Fatalf("diagnostic exposed private text or could not encode: %v", err)
	}
	fact.Name = private
	response.SupervisorControl.Upgrade.OperationID = private
	if got.ControlFact.Name != ".operation.lock" || got.OperationID != operation {
		t.Fatal("diagnostic retained mutable response authority")
	}
}

func TestColdUpgradeFailureDiagnosticSeparatesMissingAndUnreadableOwner(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		found     bool
		cause     error
		step      string
		state     string
	}{
		{name: "absent", arguments: []string{"ensure-owner", "--prepare-upgrade"}, step: "prepare_upgrade", state: "absent"},
		{name: "unreadable", found: true, cause: errors.New("PRIVATE-OWNER-OBSERVATION-CANARY"), step: "ensure_owner", state: "unreadable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := coldUpgradeFailureFacts(Response{}, test.arguments, pueue.PrivateMigrationPredecessorStopped, test.found, test.cause)
			want := coldUpgradeDiagnostic{Step: test.step, OwnerMigrationState: test.state}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("owner observation invented control/progress: got=%+v want=%+v", got, want)
			}
		})
	}
}
