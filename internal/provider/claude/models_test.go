package claude

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestModelDiscoveryDefinition(t *testing.T) {
	definition := ModelDiscovery()
	wantArgs := []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if !reflect.DeepEqual(definition.Arguments, wantArgs) || definition.Source != modelDiscoverySource || definition.NewExchange == nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
}

func TestModelDiscoveryProjectsControlInitializationAndTelemetry(t *testing.T) {
	definition := ModelDiscovery()
	telemetry := []string{
		`{"type":"system","subtype":"hook_started"}`,
		`{"type":"system","subtype":"hook_started"}`,
		`{"type":"system","subtype":"hook_started"}`,
		`{"type":"system","subtype":"hook_started"}`,
		`{"type":"system","subtype":"hook_started"}`,
		`{"type":"system","subtype":"hook_response"}`,
		`{"type":"system","subtype":"hook_response"}`,
		`{"type":"system","subtype":"hook_response"}`,
		`{"type":"system","subtype":"hook_response"}`,
		`{"type":"system","subtype":"hook_progress"}`,
		`{"type":"system","subtype":"hook_response"}`,
	}
	telemetry = append(telemetry, `{"type":"control_response","response":{"request_id":"`+modelRequestID+`","subtype":"success","response":{"models":[{"value":"claude-sonnet-4.6","displayName":"Claude Sonnet","supportedEffortLevels":["low","high"],"defaultEffort":"high"},{"value":"claude-haiku","supportsEffort":false}],"account":{"email":"secret@example.invalid","token":"secret-token"}}}}`)
	help := []byte("  --effort <level>                      Effort level for the current session\n                                        (low, medium, high, xhigh, max)\n")
	catalog, err := definition.Project([]byte(strings.Join(telemetry, "\n")+"\n"), nil, help, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || !catalog.Complete || len(catalog.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if catalog.Models[0].ID != "claude-sonnet-4.6" || catalog.Models[0].DefaultEffort != "high" || catalog.Models[1].Efforts == nil || len(catalog.Models[1].Efforts) != 0 {
		t.Fatalf("unexpected Claude model facts: %+v", catalog.Models)
	}
	if !reflect.DeepEqual(catalog.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("wrapped effort help was not parsed: %#v", catalog.Efforts)
	}
	data, marshalErr := json.Marshal(catalog)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(data), "secret@example.invalid") || strings.Contains(string(data), "secret-token") {
		t.Fatal("Claude account metadata leaked into model facts")
	}
}

func TestModelDiscoveryAcceptsTelemetryAndCorrelatedControlError(t *testing.T) {
	exchange := ModelDiscovery().NewExchange()
	if _, err := exchange.Start(); err != nil {
		t.Fatal(err)
	}
	if replies, done, err := exchange.Accept([]byte(`{"type":"system","subtype":"hook_started"}`)); err != nil || done || len(replies) != 0 {
		t.Fatalf("telemetry was not ignored: %q %v %v", replies, done, err)
	}
	if replies, done, err := exchange.Accept([]byte(`{"type":"control_response","response":{"request_id":"` + modelRequestID + `","subtype":"success","response":{}}}`)); err != nil || !done || len(replies) != 0 {
		t.Fatalf("success response did not finish exchange: %q %v %v", replies, done, err)
	}

	errorExchange := ModelDiscovery().NewExchange()
	if _, err := errorExchange.Start(); err != nil {
		t.Fatal(err)
	}
	errorLine := []byte(`{"type":"control_response","response":{"request_id":"` + modelRequestID + `","subtype":"error","error":"authentication required"}}`)
	if replies, done, err := errorExchange.Accept(errorLine); err != nil || !done || len(replies) != 0 {
		t.Fatalf("correlated control error did not finish exchange: %q %v %v", replies, done, err)
	}
	if _, _, err := errorExchange.Accept(errorLine); err == nil {
		t.Fatal("duplicate terminal control response was accepted")
	}
	uncorrelated := ModelDiscovery().NewExchange()
	if _, err := uncorrelated.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uncorrelated.Accept([]byte(`{"type":"control_response","response":{"request_id":"other","subtype":"success","response":{}}}`)); err == nil {
		t.Fatal("uncorrelated control response was accepted")
	}
}

func TestModelDiscoveryClassifiesNativeFailure(t *testing.T) {
	failed, err := ModelDiscovery().Project(nil, []byte("permission denied"), nil, false)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("permission failure was misclassified: %+v %v", failed, err)
	}
	blocked, err := ModelDiscovery().Project(nil, []byte("authentication required"), nil, false)
	if err != nil || blocked.Status != "blocked" {
		t.Fatalf("auth failure was misclassified: %+v %v", blocked, err)
	}
}

func TestModelDiscoveryRejectsWrongCorrelatedIDAfterSuccess(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"control_response","response":{"request_id":"delegation-layer-model-discovery","subtype":"success","response":{"models":[{"value":"claude/one"}]}}}`,
		`{"type":"control_response","response":{"request_id":"other-request","subtype":"success","response":{"models":[]}}}`,
	}, "\n")
	if _, err := ModelDiscovery().Project([]byte(stdout), nil, nil, true); err == nil {
		t.Fatal("projection accepted a post-completion response with the wrong request id")
	}
}
