package ccr

import (
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestModelDiscoveryProjectsAliasesAndUnknownEffort(t *testing.T) {
	catalog, err := projectModels([]byte(`{"schema_version":1,"models":[{"alias":"local","provider":"gateway","provider_model":"model-a","compatibility":"degraded","effective_capabilities":{"values":{"supports_thinking":true}}}]}`), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || !catalog.Complete || len(catalog.Models) != 1 || catalog.Models[0].ID != "local" || catalog.Models[0].Efforts != nil || catalog.Efforts != nil {
		t.Fatalf("catalog=%+v", catalog)
	}
}

func TestModelDiscoveryEmptyAndInvalid(t *testing.T) {
	catalog, err := projectModels([]byte(`{"schema_version":1,"models":[]}`), nil, nil, true)
	if err != nil || catalog.Status != "unavailable" {
		t.Fatalf("empty: %+v %v", catalog, err)
	}
	for _, invalid := range []string{`{}`, `{"schema_version":2,"models":[]}`, `{"schema_version":1,"models":null}`, `{"schema_version":1,"models":[{"alias":"same"},{"alias":"same"}]}`, strings.Repeat(" ", int(commonprovider.MaxInspectionOutput)+1)} {
		if _, invalidErr := projectModels([]byte(invalid), nil, nil, true); invalidErr == nil {
			t.Fatal("invalid listing accepted")
		}
	}
	catalog, err = projectModels(nil, []byte("unknown flag: --json"), nil, false)
	if err != nil || catalog.Status != "unavailable" {
		t.Fatalf("interface failure: %+v %v", catalog, err)
	}
}
