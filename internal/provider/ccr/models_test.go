package ccr

import (
	"reflect"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestModelDiscoveryProjectsAliasesAndUnknownEffort(t *testing.T) {
	definition := ModelDiscovery()
	if !reflect.DeepEqual(definition.Arguments, []string{"model", "list"}) || definition.Source != modelSource || !reflect.DeepEqual(definition.Capability.VersionArgs, []string{"version"}) || definition.Capability.RequiredFlags != nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
	catalog, err := definition.Project([]byte("local\tprovider=gateway\tmodel=model-a\tcompat=degraded\n"), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || !catalog.Complete || len(catalog.Models) != 1 || catalog.Models[0].ID != "local" || catalog.Models[0].Efforts != nil || catalog.Efforts != nil {
		t.Fatalf("catalog=%+v", catalog)
	}
}

func TestModelDiscoveryEmptyAndInvalid(t *testing.T) {
	catalog, err := projectModels([]byte("No model aliases configured.\n"), nil, nil, true)
	if err != nil || catalog.Status != "unavailable" {
		t.Fatalf("empty: %+v %v", catalog, err)
	}
	for _, invalid := range []string{
		"{\"schema_version\":1,\"models\":[]}",
		"local provider=gateway\tmodel=model-a\tcompat=degraded",
		"local\tprovider=gateway\tmodel=model-a",
		string(make([]byte, int(commonprovider.MaxInspectionOutput)+1)),
	} {
		if _, invalidErr := projectModels([]byte(invalid), nil, nil, true); invalidErr == nil {
			t.Fatal("invalid listing accepted")
		}
	}
	catalog, err = projectModels(nil, []byte("unknown command: model"), nil, false)
	if err != nil || catalog.Status != "unavailable" {
		t.Fatalf("interface failure: %+v %v", catalog, err)
	}
}
