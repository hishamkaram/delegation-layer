package opencode

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestModelDiscoveryDefinition(t *testing.T) {
	definition := ModelDiscovery()
	if !reflect.DeepEqual(definition.Arguments, []string{"models", "--standalone"}) || definition.Source != "opencode models --standalone" || definition.NewExchange != nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
	if !reflect.DeepEqual(definition.Capability.HelpArgs, []string{"models"}) || !reflect.DeepEqual(definition.Capability.RequiredFlags, []string{"--standalone"}) {
		t.Fatalf("discovery must require only the standalone flag: %+v", definition.Capability)
	}
}

func TestModelDiscoveryProjectsPlainIDListWithUnknownEfforts(t *testing.T) {
	stdout := "opencode-go/glm-5.2\nopencode/big-pickle\n\nopenrouter/anthropic/claude-opus-5.5\n"
	catalog, err := ModelDiscovery().Project([]byte(stdout), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || catalog.ReasonCode != "ok" || !catalog.Complete || catalog.Source != "opencode models --standalone" || len(catalog.Models) != 3 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	for index, id := range []string{"opencode-go/glm-5.2", "opencode/big-pickle", "openrouter/anthropic/claude-opus-5.5"} {
		model := catalog.Models[index]
		if model.ID != id || model.Name != "" || model.Efforts != nil || model.DefaultEffort != "" {
			t.Fatalf("plain listing invented metadata for %q: %+v", id, model)
		}
	}
}

func TestModelDiscoveryProjectsPlainListWithoutTrailingNewline(t *testing.T) {
	models, err := parseModelListing([]byte("opencode/big-pickle"))
	if err != nil || len(models) != 1 || models[0].ID != "opencode/big-pickle" {
		t.Fatalf("unterminated final ID was mishandled: %+v %v", models, err)
	}
}

func TestModelDiscoveryAcceptsMixedPlainAndMetadataRecords(t *testing.T) {
	stdout := "plain/one\nmeta/two\n{\"name\": \"Two\", \"variants\": {\"low\": {}}}\nplain/three\n{\"displayName\": \"Three\"}\nplain/four"
	models, err := parseModelListing([]byte(stdout))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 {
		t.Fatalf("unexpected models: %+v", models)
	}
	if models[0].ID != "plain/one" || models[0].Name != "" || models[0].Efforts != nil {
		t.Fatalf("plain model gained metadata: %+v", models[0])
	}
	if models[1].ID != "meta/two" || models[1].Name != "Two" || !reflect.DeepEqual(models[1].Efforts, []string{"low"}) {
		t.Fatalf("metadata was not applied to its own model: %+v", models[1])
	}
	if models[2].ID != "plain/three" || models[2].Name != "Three" || models[2].Efforts != nil {
		t.Fatalf("display name metadata mishandled: %+v", models[2])
	}
	if models[3].ID != "plain/four" || models[3].Name != "" {
		t.Fatalf("trailing plain model mishandled: %+v", models[3])
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

func TestModelDiscoveryRejectsInvalidListings(t *testing.T) {
	tests := map[string]string{
		"duplicate plain id":          "a/b\nc/d\na/b\n",
		"duplicate id with metadata":  "a/b\n{\"name\": \"A\"}\na/b\n",
		"metadata without id":         "{\"name\": \"A\"}\n",
		"consecutive metadata":        "a/b\n{\"name\": \"A\"}\n{\"name\": \"B\"}\n",
		"multiple tokens on one line": "a/b c/d\n",
		"non-object metadata":         "a/b\n{\"variants\": []}\n",
		"truncated metadata":          "a/b\n{\"name\": \"A\"",
		"non-object variants":         "a/b\n{\"variants\": \"high\"}\n",
	}
	for name, stdout := range tests {
		t.Run(name, func(t *testing.T) {
			if models, err := parseModelListing([]byte(stdout)); err == nil {
				t.Fatalf("invalid listing was accepted: %+v", models)
			}
		})
	}
}

func TestModelDiscoveryBoundsListing(t *testing.T) {
	var tooMany strings.Builder
	for index := range 4097 {
		if _, err := fmt.Fprintf(&tooMany, "p/m%d\n", index); err != nil {
			t.Fatalf("build model listing: %v", err)
		}
	}
	if _, err := parseModelListing([]byte(tooMany.String())); err == nil {
		t.Fatal("model count bound was not enforced")
	}
	if _, err := parseModelListing([]byte(strings.Repeat("x", int(commonprovider.MaxInspectionOutput)+1))); err == nil {
		t.Fatal("output bound was not enforced")
	}
}

func TestModelDiscoveryEmptyPlainListingIsUnavailable(t *testing.T) {
	catalog, err := projectModels([]byte("\n \n"), nil, nil, true)
	if err != nil || catalog.Status != "unavailable" || catalog.ReasonCode != "models_unavailable" || catalog.Complete {
		t.Fatalf("empty listing was misclassified: %+v %v", catalog, err)
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
