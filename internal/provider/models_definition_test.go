package provider

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestModelDiscoverySnapshotPreservesHistoricalShape(t *testing.T) {
	definition := inspectionTestDefinition()
	// This is the authored pre-discovery wire representation, independent of
	// the current snapshot implementation. Its bytes bind persisted records.
	historical := []byte(`{"revision":"native-metadata-v1","executable":"/usr/bin/security","executable_sha256":"e81d3b0e9d82feaaf5f6e55bdff24731d7eee08632ffa63801e6397290c5d20a","arguments":["fixed-argument"],"directory":"/workspace","environment":["LANG=C"],"output_limit":1048576}` + "\n")
	snapshot, digest, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := task.MarshalCanonical(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(historical) || digest != task.ComputeSHA256(historical) {
		t.Fatalf("absent discovery changed historical binding: %s digest=%s", encoded, digest)
	}
	definition.Revision = ModelsRevision
	definition.Arguments = nil
	definition.Project = nil
	definition.Models = &ModelDiscoveryDefinition{Arguments: []string{"models"}, Source: "native", Project: func([]byte, []byte, []byte, bool) (ModelCatalog, error) {
		t.Fatal("snapshot invoked projector")
		return ModelCatalog{}, nil
	}}
	snapshot, _, err = definition.Snapshot()
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
