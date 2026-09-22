package antigravity

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestModelDiscoveryProjectsObservedTSVAndEfforts(t *testing.T) {
	definition := ModelDiscovery()
	if !reflect.DeepEqual(definition.Arguments, []string{"models"}) || definition.Source != modelDiscoverySource || definition.NewExchange != nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
	help := []byte("  --effort Reasoning effort for the current CLI session (low|medium|high)\n")
	stdout := []byte("\nFetching available models...\n" +
		"gemini-3.8-flash-high\tGemini Flash High\n" +
		"gemini-3.8-flash-low\tGemini Flash Low\n")
	catalog, err := definition.Project(stdout, nil, help, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || !catalog.Complete || len(catalog.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if catalog.Models[0].ID != "gemini-3.8-flash-high" || catalog.Models[0].Name != "Gemini Flash High" {
		t.Fatalf("native model slug was changed: %+v", catalog.Models[0])
	}
	if !reflect.DeepEqual(catalog.Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("unexpected efforts: %#v", catalog.Efforts)
	}
}

func TestModelDiscoveryRejectsMalformedTSV(t *testing.T) {
	_, err := parseModelsTSV([]byte("Fetching available models...\nmodel-without-name\n"))
	if err == nil {
		t.Fatal("malformed agy row was accepted")
	}
}

func TestModelDiscoveryClassifiesAuthAndNativeFailure(t *testing.T) {
	blocked, err := projectModels(nil, []byte("authentication required: secret-token"), nil, false)
	if err != nil || blocked.Status != "blocked" || blocked.Complete || len(blocked.Models) != 0 {
		t.Fatalf("unexpected blocked catalog: %+v %v", blocked, err)
	}
	data, marshalErr := json.Marshal(blocked)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatal("native diagnostic leaked into catalog")
	}
	failed, err := projectModels(nil, []byte("permission denied"), nil, false)
	if err != nil || failed.Status != "failed" || failed.ReasonCode != "native_command_failed" {
		t.Fatalf("permission failure was misclassified: %+v %v", failed, err)
	}
}
