package contract

import (
	"encoding/json"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDescriptionSerializesDiscoveryMetadata(t *testing.T) {
	description := Description{
		Interaction:      task.InteractionCapabilities{},
		ID:               "alpha",
		SupportedModes:   []string{"read-only"},
		SupportedOptions: []string{OptionModel, OptionEffort},
		RequiredOptions:  []string{OptionModel},
		Continuation:     ContinuationNative,
		ReadOnly: ReadOnlyCapability{
			Supported:   true,
			Mechanism:   ReadOnlyNativeSandbox,
			Containment: ReadOnlyProviderOwned,
		},
		Runtime:      RuntimeCapability{HelpArgs: []string{"exec"}, RequiredFlags: []string{"--json"}},
		Discoverable: true,
	}
	encoded, err := json.Marshal(description)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := fields["Discoverable"]; ok {
		t.Fatalf("Discoverable must stay out of the providers response: %s", encoded)
	}
	assertField(t, fields, "id", `"alpha"`)
	assertField(t, fields, "continuation", `"native"`)
	assertField(t, fields, "supported_options", `["model","effort"]`)
	assertField(t, fields, "required_options", `["model"]`)
	assertField(t, fields, "read_only", `{"supported":true,"mechanism":"native-sandbox","containment":"provider-owned"}`)
	assertField(t, fields, "runtime", `{"help_args":["exec"],"required_flags":["--json"]}`)
}

func TestDescriptionOmitsEmptyOptionalMetadata(t *testing.T) {
	encoded, err := json.Marshal(Description{ID: "beta", Continuation: ContinuationUnsupported})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := fields["required_options"]; ok {
		t.Fatalf("empty required_options was serialized: %s", encoded)
	}
	assertField(t, fields, "read_only", `{"supported":false}`)
	assertField(t, fields, "runtime", `{"required_flags":null}`)
}

func assertField(t *testing.T, fields map[string]json.RawMessage, name, want string) {
	t.Helper()
	got, ok := fields[name]
	if !ok {
		t.Fatalf("field %q is absent", name)
	}
	if string(got) != want {
		t.Fatalf("field %q = %s, want %s", name, got, want)
	}
}
