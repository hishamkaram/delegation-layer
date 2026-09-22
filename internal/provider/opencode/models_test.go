package opencode

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestModelDiscoveryDefinition(t *testing.T) {
	definition := ModelDiscovery()
	if !reflect.DeepEqual(definition.Arguments, []string{"models", "--verbose"}) || definition.Source != modelDiscoverySource || definition.NewExchange != nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
}

func TestModelDiscoveryProjectsVerboseIDAndVariants(t *testing.T) {
	definition := ModelDiscovery()
	stdout := strings.TrimSpace(`openai/gpt-5
{
  "id": "gpt-5",
  "providerID": "openai",
  "name": "GPT 5",
  "headers": {"Authorization": "Bearer secret-token"},
  "options": {"credential": "secret-token"},
  "variants": {
    "high": {"reasoningEffort": "high"},
    "low": {"reasoningEffort": "low"}
  }
}

opencode/big-pickle
{
  "name": "Big Pickle",
  "variants": {}
}
`) + "\n"
	catalog, err := definition.Project([]byte(stdout), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || !catalog.Complete || len(catalog.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if catalog.Models[0].ID != "openai/gpt-5" || catalog.Models[0].Name != "GPT 5" {
		t.Fatalf("unexpected first model: %+v", catalog.Models[0])
	}
	if !reflect.DeepEqual(catalog.Models[0].Efforts, []string{"high", "low"}) {
		t.Fatalf("unexpected variants: %#v", catalog.Models[0].Efforts)
	}
	if catalog.Models[1].Efforts == nil || len(catalog.Models[1].Efforts) != 0 {
		t.Fatalf("empty variant object was not preserved: %#v", catalog.Models[1].Efforts)
	}
	data, marshalErr := json.Marshal(catalog)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(data), "secret-token") || strings.Contains(string(data), "headers") || strings.Contains(string(data), "options") {
		t.Fatalf("verbose metadata leaked into catalog: %s", data)
	}
}

func TestModelDiscoveryRejectsMalformedVerboseRecord(t *testing.T) {
	_, err := parseModelListing([]byte("openai/gpt-5\n{\"variants\": [}\n"))
	if err == nil {
		t.Fatal("malformed OpenCode metadata was accepted")
	}
}

func TestModelDiscoveryClassifiesNativeFailure(t *testing.T) {
	failed, err := projectModels(nil, []byte("permission denied"), nil, false)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("permission failure was misclassified: %+v %v", failed, err)
	}
	blocked, err := projectModels(nil, []byte("authentication required"), nil, false)
	if err != nil || blocked.Status != "blocked" {
		t.Fatalf("auth failure was misclassified: %+v %v", blocked, err)
	}
}
