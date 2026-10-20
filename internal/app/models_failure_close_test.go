package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestJoinCloseFailureKeepsPrimaryStageAndCauses(t *testing.T) {
	closeCause := fmt.Errorf("close %s: %w", modelsFailureCanary, errors.New("descriptor busy"))
	primary := admission.WithStage("start-supervisor", errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, errors.New("probe failed")))

	joined := joinCloseFailure(primary, "close-supervisor", closeCause)
	failure := dispatchFailure(joined, "not_created")
	if failure.Stage != "start-supervisor" || failure.Code != "supervisor_upgrade_required" || failure.NextAction != "run_supervisor_migrate" {
		t.Fatalf("a close error replaced the primary failure: %+v", failure)
	}
	if !errors.Is(joined, pueue.ErrPrivateSupervisorMigrationRequired) || !errors.Is(joined, closeCause) {
		t.Fatalf("join dropped a cause: %v", joined)
	}
	if message := modelDiscoveryPublicError(joined, failure); strings.Contains(message, modelsFailureCanary) {
		t.Fatalf("public message exposed the close cause: %q", message)
	}
}

func TestJoinCloseFailureAloneIsClassifiedByItsCloseStage(t *testing.T) {
	closeCause := errors.New("lock busy")
	for _, stage := range []string{"close-supervisor", "close-state"} {
		failure := dispatchFailure(joinCloseFailure(nil, stage, closeCause), "not_created")
		if failure.Stage != stage || failure.NextAction != "stop_and_report" {
			t.Fatalf("%s fault lost its stage: %+v", stage, failure)
		}
	}
	if err := joinCloseFailure(nil, "close-state", nil); err != nil {
		t.Fatalf("successful close produced an error: %v", err)
	}
}

func TestModelsInspectionFailureCarriesBoundedContext(t *testing.T) {
	taskID := strings.Repeat("a", 32)
	err := admission.WithStage("inspect-provider", admission.WithInspectionFailureContext(
		errors.Join(inspection.ErrAdmissionExpired, fmt.Errorf("%s /private/user/state/inspections/%s", modelsFailureCanary, taskID)),
		taskID, time.Now().Add(-time.Second),
	))
	failure := dispatchFailure(err, "not_created")
	catalog, exit := modelDiscoveryFailureCatalog(err, failure)
	response := ModelsResponse{SchemaVersion: OutputSchemaVersion, Command: "models", Provider: "fixture:test", ModelCatalog: catalog, Failure: &failure, Error: modelDiscoveryPublicError(err, failure)}
	if exit != 1 || catalog.Status != "failed" || catalog.ReasonCode != "discovery_timeout" {
		t.Fatalf("inspection deadline changed the catalog contract: exit=%d %+v", exit, catalog)
	}
	if failure.Stage != "inspect-provider" || failure.Code != "provider_preflight_failed" || failure.Inspection == nil ||
		failure.Inspection.Class != "admission_deadline_expired" || failure.Inspection.DeadlineState != "expired" || failure.Inspection.JournalTaskID != taskID {
		t.Fatalf("inspection failure lost bounded context: %+v inspection=%+v", failure, failure.Inspection)
	}
	data, human := encodeModelsResponses(t, response)
	for _, output := range []string{data, human} {
		if strings.Contains(output, modelsFailureCanary) || strings.Contains(output, "/private/") {
			t.Fatalf("inspection output exposed raw cause: %s", output)
		}
	}
	if !strings.Contains(human, "inspection: admission_deadline_expired (deadline: expired)\n") {
		t.Fatalf("human output lacks inspection context:\n%s", human)
	}
}

func encodeModelsResponses(t *testing.T, response ModelsResponse) (jsonText, human string) {
	t.Helper()
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	if err = writeModelsHuman(&buffer, response); err != nil {
		t.Fatal(err)
	}
	return string(data), buffer.String()
}

func TestModelsSuccessCarriesNoFailureFields(t *testing.T) {
	response := ModelsResponse{
		SchemaVersion: OutputSchemaVersion, Command: "models", Provider: "fixture:test",
		ModelCatalog: commonprovider.ModelCatalog{Status: "available", ReasonCode: "ok", Complete: true, Source: "fixture", Models: []commonprovider.ModelInfo{}},
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"failure"`, `"error"`} {
		if strings.Contains(string(data), key) {
			t.Fatalf("success response carries %s: %s", key, data)
		}
	}
	var human bytes.Buffer
	if err = writeModelsHuman(&human, response); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(human.String(), "failure:") || strings.Contains(human.String(), "error:") {
		t.Fatalf("success human output carries failure lines:\n%s", human.String())
	}
}

// The expected text is spelled out so a change to the shared default message
// is a visible contract change, not a silent follow-the-helper update.
const neutralFailureMessage = "Delegate could not complete the operation."

func TestModelsGenericFailureUsesNeutralMessage(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "unmapped stage", err: admission.WithStage("open-state", fmt.Errorf("%s: %w", modelsFailureCanary, errors.New("state busy"))), code: "dispatch_failed"},
		{name: "provider preparation", err: admission.WithStage("prepare-provider", fmt.Errorf("%s: %w", modelsFailureCanary, errors.New("rejected"))), code: "provider_preparation_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := dispatchFailure(test.err, "not_created")
			catalog, exit := modelDiscoveryFailureCatalog(test.err, failure)
			response := ModelsResponse{SchemaVersion: OutputSchemaVersion, Command: "models", Provider: "fixture:test", ModelCatalog: catalog, Failure: &failure, Error: modelDiscoveryPublicError(test.err, failure)}
			if failure.Code != test.code || response.Error != neutralFailureMessage {
				t.Fatalf("failure = %+v error = %q, want code %s and %q", failure, response.Error, test.code, neutralFailureMessage)
			}
			if exit != 1 || catalog.Status != "failed" || catalog.ReasonCode != "discovery_failed" || catalog.Source != "none" {
				t.Fatalf("generic failure changed the catalog contract: exit=%d %+v", exit, catalog)
			}
			assertNeutralModelsOutput(t, response)
		})
	}
}

func assertNeutralModelsOutput(t *testing.T, response ModelsResponse) {
	t.Helper()
	data, human := encodeModelsResponses(t, response)
	if !strings.Contains(data, `"error":"`+neutralFailureMessage+`"`) || !strings.Contains(human, "error: "+neutralFailureMessage+"\n") {
		t.Fatalf("neutral message missing:\n%s\n%s", data, human)
	}
	for _, output := range []string{data, human} {
		if strings.Contains(output, modelsFailureCanary) || strings.Contains(output, "dispatch.") {
			t.Fatalf("output leaked a cause or the command-specific text: %s", output)
		}
	}
}
