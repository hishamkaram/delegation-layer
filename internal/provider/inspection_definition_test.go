package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func inspectionTestDefinition() *InspectionDefinition {
	return &InspectionDefinition{
		Revision: "native-metadata-v1", Executable: "/usr/bin/security", ExecutableSHA256: task.ComputeSHA256([]byte("helper")),
		Arguments: []string{"fixed-argument"}, Directory: "/workspace", Environment: []string{"LANG=C"}, OutputLimit: MaxInspectionOutput,
		Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"eligible":true}`), nil },
	}
}

func TestInspectionSnapshotPinsDescriptionWithoutEvaluatingCallbacks(t *testing.T) {
	definition := inspectionTestDefinition()
	definition.Project = nil
	definition.Remote = &HTTPInspectionDefinition{
		URL: "https://provider.invalid/settings", Headers: map[string]string{"Accept": "application/json"},
		Authorization: func([]byte) (string, bool, error) { t.Fatal("snapshot requested a credential"); return "", false, nil },
		Project: func([]byte, int, []byte) (json.RawMessage, error) {
			t.Fatal("snapshot ran projection")
			return nil, nil
		},
	}
	snapshot, digest, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Authorization") || strings.Contains(string(data), "Project") {
		t.Fatal("serialized callback implementation")
	}
	if digest != task.ComputeSHA256(data) {
		t.Fatal("digest did not bind serialized effect description")
	}
	definition.Arguments[0] = "changed"
	definition.Environment[0] = "CHANGED=yes"
	definition.Remote.Headers["Accept"] = "changed"
	definition.Remote.URL = "https://changed.invalid/settings"
	if snapshot.Arguments[0] != "fixed-argument" || snapshot.Environment[0] != "LANG=C" || snapshot.Remote.Headers["Accept"] != "application/json" || snapshot.Remote.URL != "https://provider.invalid/settings" {
		t.Fatal("effect snapshot aliases mutable factory storage")
	}
	_, changed, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if changed == digest {
		t.Fatal("changed effect retained its digest")
	}
	definition.Project = inspectionTestDefinition().Project
	if _, _, err = definition.Snapshot(); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("ambiguous projector routing accepted: %v", err)
	}
}

func TestInspectionSnapshotRejectsUnboundEffects(t *testing.T) {
	tests := []struct {
		name   string
		change func(*InspectionDefinition)
	}{
		{"relative executable", func(d *InspectionDefinition) { d.Executable = "security" }},
		{"missing hash", func(d *InspectionDefinition) { d.ExecutableSHA256 = "" }},
		{"ambient environment", func(d *InspectionDefinition) { d.Environment = nil }},
		{"duplicate environment", func(d *InspectionDefinition) { d.Environment = []string{"LANG=C", "LANG=other"} }},
		{"invalid argument encoding", func(d *InspectionDefinition) { d.Arguments = []string{string([]byte{255})} }},
		{"nul argument", func(d *InspectionDefinition) { d.Arguments = []string{"bad\x00argument"} }},
		{"unbounded output", func(d *InspectionDefinition) { d.OutputLimit = MaxInspectionOutput + 1 }},
		{"missing projector", func(d *InspectionDefinition) { d.Project = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := inspectionTestDefinition()
			test.change(d)
			if _, _, err := d.Snapshot(); !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("invalid effect accepted: %v", err)
			}
		})
	}
}

func TestInspectionSnapshotRejectsUnsafeRemoteDescriptions(t *testing.T) {
	tests := []struct {
		name   string
		change func(*HTTPInspectionDefinition)
	}{
		{"plaintext", func(r *HTTPInspectionDefinition) { r.URL = "http://provider.invalid/settings" }},
		{"URL credentials", func(r *HTTPInspectionDefinition) { r.URL = "https://name:password@provider.invalid/settings" }},
		{"missing authorization callback", func(r *HTTPInspectionDefinition) { r.Authorization = nil }},
		{"missing projector", func(r *HTTPInspectionDefinition) { r.Project = nil }},
		{"fixed credentials", func(r *HTTPInspectionDefinition) { r.Headers["Authorization"] = "fixture-secret" }},
		{"header injection", func(r *HTTPInspectionDefinition) { r.Headers["Accept"] = "application/json\r\nInjected: true" }},
		{"duplicate header", func(r *HTTPInspectionDefinition) { r.Headers["accept"] = "text/plain" }},
		{"invalid header name", func(r *HTTPInspectionDefinition) { r.Headers["bad:name"] = "value" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := inspectionTestDefinition()
			d.Project = nil
			d.Remote = &HTTPInspectionDefinition{URL: "https://provider.invalid/settings", Headers: map[string]string{"Accept": "application/json"}, Authorization: func([]byte) (string, bool, error) { return "", false, nil }, Project: func([]byte, int, []byte) (json.RawMessage, error) { return nil, nil }}
			test.change(d.Remote)
			if _, _, err := d.Snapshot(); !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("unsafe remote description accepted: %v", err)
			}
		})
	}
}
