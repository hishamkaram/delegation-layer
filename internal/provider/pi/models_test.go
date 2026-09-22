package pi

import (
	"reflect"
	"testing"
)

func TestModelDiscoveryProjectsObservedTable(t *testing.T) {
	definition := ModelDiscovery()
	if !reflect.DeepEqual(definition.Arguments, []string{"--list-models"}) || definition.Source != modelDiscoverySource || definition.NewExchange != nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
	help := []byte("  --thinking <level>             Set thinking level: off, minimal, low, medium, high, xhigh, max\n")
	stdout := []byte("provider    model                                               context  max-out  thinking  images\n" +
		"openrouter  anthropic/claude-sonnet-4.6                         1M       64K     yes       yes\n" +
		"openrouter  openai/gpt-5                                       400K     128K    yes       no\n")
	catalog, err := definition.Project(stdout, nil, help, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || !catalog.Complete || len(catalog.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if got := catalog.Models[0].ID; got != "openrouter/anthropic/claude-sonnet-4.6" {
		t.Fatalf("unexpected exact Pi model selector %q", got)
	}
	if catalog.Models[0].Efforts != nil {
		t.Fatalf("yes/no capability column became effort metadata: %#v", catalog.Models[0].Efforts)
	}
	if !reflect.DeepEqual(catalog.Efforts, []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("unexpected thinking levels: %#v", catalog.Efforts)
	}
}

func TestModelDiscoveryRejectsUnknownTableShape(t *testing.T) {
	_, _, err := parseModelTable([]byte("provider model\nopenai gpt-5\n"))
	if err == nil {
		t.Fatal("unknown Pi table shape was accepted")
	}
}

func TestModelDiscoveryClassifiesPermissionAndAuth(t *testing.T) {
	failed, err := projectModels(nil, []byte("permission denied"), nil, false)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("permission failure was not failed: %+v %v", failed, err)
	}
	blocked, err := projectModels(nil, []byte("login required"), nil, false)
	if err != nil || blocked.Status != "blocked" || len(blocked.Models) != 0 {
		t.Fatalf("auth failure was not blocked: %+v %v", blocked, err)
	}
}
