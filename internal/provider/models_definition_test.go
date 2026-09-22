package provider

import (
	"testing"
)

func TestModelDiscoverySnapshotPreservesHistoricalShape(t *testing.T) {
	definition := inspectionTestDefinition()
	_, historicalDigest, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	definition.Models = nil
	_, unchangedDigest, err := definition.Snapshot()
	if err != nil || historicalDigest != unchangedDigest {
		t.Fatal("absent discovery changed historical binding")
	}
	definition.Revision = ModelsRevision
	definition.Arguments = nil
	definition.Project = nil
	definition.Models = &ModelDiscoveryDefinition{Arguments: []string{"models"}, Source: "native", Project: func([]byte, []byte, []byte, bool) (ModelCatalog, error) {
		t.Fatal("snapshot invoked projector")
		return ModelCatalog{}, nil
	}}
	snapshot, _, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	definition.Models.Arguments[0] = "changed"
	if snapshot.Models.Arguments[0] != "models" {
		t.Fatal("snapshot aliases discovery arguments")
	}
	definition.Runtime = &RuntimeProbeDefinition{}
	if _, _, err = definition.Snapshot(); err == nil {
		t.Fatal("ambiguous runtime/discovery accepted")
	}
}
