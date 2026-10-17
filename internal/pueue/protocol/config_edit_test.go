package protocol

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestConfigWithFileEditModeDereferencesClientAlias(t *testing.T) {
	source := []byte("profiles:\n  base:\n    client: &base\n      show_confirmation_questions: false\n      edit_mode: toml\nclient: *base\n")
	var before rawSettings
	if err := yaml.Unmarshal(source, &before); err != nil {
		t.Fatal(err)
	}
	data, err := ConfigWithFileEditMode(source)
	if err != nil {
		t.Fatal(err)
	}
	var after rawSettings
	if err = yaml.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.Client == nil || after.Client.EditMode == nil || *after.Client.EditMode != "files" {
		t.Fatalf("top-level aliased client did not select file mode: %s", data)
	}
	if before.Profiles["base"].Client.EditMode == nil || *before.Profiles["base"].Client.EditMode != "toml" || after.Profiles["base"].Client.EditMode == nil || *after.Profiles["base"].Client.EditMode != "toml" {
		t.Fatalf("dereferencing top-level client changed its profile source: before=%+v after=%+v", before.Profiles["base"].Client, after.Profiles["base"].Client)
	}
}

func TestConfigWithFileEditModeDetachesAnchoredTopLevelClient(t *testing.T) {
	source := []byte("client: &base\n  show_confirmation_questions: false\n  edit_mode: toml\nprofiles:\n  base:\n    client: *base\n")
	var before rawSettings
	if err := yaml.Unmarshal(source, &before); err != nil {
		t.Fatal(err)
	}
	data, err := ConfigWithFileEditMode(source)
	if err != nil {
		t.Fatal(err)
	}
	var after rawSettings
	if err = yaml.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.Client == nil || after.Client.EditMode == nil || *after.Client.EditMode != "files" {
		t.Fatalf("top-level anchored client did not select file mode: %s", data)
	}
	if before.Profiles["base"].Client.EditMode == nil || *before.Profiles["base"].Client.EditMode != "toml" || after.Profiles["base"].Client.EditMode == nil || *after.Profiles["base"].Client.EditMode != "toml" {
		t.Fatalf("changing the top-level client changed its profile source: before=%+v after=%+v", before.Profiles["base"].Client, after.Profiles["base"].Client)
	}
}

func TestConfigWithFileEditModeRewritesEditModeScalarAlias(t *testing.T) {
	source := []byte("profiles:\n  base:\n    client:\n      edit_mode: &mode toml\nclient:\n  show_confirmation_questions: false\n  edit_mode: *mode\n")
	before, err := ParseConfig(source)
	if err != nil {
		t.Fatalf("fixture config is invalid: %v", err)
	}
	data, err := ConfigWithFileEditMode(source)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("migrated config is invalid: %v\n%s", err, data)
	}
	var afterRaw rawSettings
	if err = yaml.Unmarshal(data, &afterRaw); err != nil {
		t.Fatal(err)
	}
	if before.Client.EditMode != "toml" || after.Client.EditMode != "files" || afterRaw.Profiles["base"].Client.EditMode == nil || *afterRaw.Profiles["base"].Client.EditMode != "toml" {
		t.Fatalf("changing the client alias changed profile config: before=%+v after=%+v", before, afterRaw.Profiles["base"].Client)
	}
}
