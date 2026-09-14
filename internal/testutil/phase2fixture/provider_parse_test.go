package phase2fixture

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestParseProviderConfigUsesProvidedBytes(t *testing.T) {
	dir := t.TempDir()
	first := testProviderConfig(dir, "success")
	first.Argv = []string{"first-literal"}
	firstData, err := task.MarshalCanonical(first)
	if err != nil {
		t.Fatal(err)
	}

	second := first
	second.Argv = []string{"second-literal"}
	configPath := writeProviderConfig(t, dir, second)
	parsed, err := ParseProviderConfig(firstData)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(parsed.Argv, first.Argv) {
		t.Fatalf("parsed supplied bytes=%q want=%q", parsed.Argv, first.Argv)
	}

	loaded, err := LoadProviderConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(loaded.Argv, second.Argv) {
		t.Fatalf("loaded file bytes=%q want=%q", loaded.Argv, second.Argv)
	}
}
