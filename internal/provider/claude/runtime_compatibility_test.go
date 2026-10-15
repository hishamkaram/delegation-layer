package claude

import (
	"slices"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func requireRuntimeProbeDefinition(t *testing.T, definition *commonprovider.InspectionDefinition, wantFlags []string) string {
	t.Helper()
	if definition == nil || definition.Runtime == nil {
		t.Fatalf("runtime inspection=%+v", definition)
	}
	if !slices.Equal(definition.Runtime.RequiredFlags, wantFlags) {
		t.Fatalf("runtime required flags=%q want=%q", definition.Runtime.RequiredFlags, wantFlags)
	}
	if len(definition.Runtime.VersionArgs) != 0 || len(definition.Runtime.HelpArgs) != 0 {
		t.Fatalf("runtime probe has unexpected version/help args: %+v", definition.Runtime)
	}
	expected, err := commonprovider.NewRuntimeInspectionDefinition(
		commonprovider.CLIInfo{Path: definition.Executable, SHA256: definition.ExecutableSHA256},
		definition.Directory,
		definition.Environment,
		commonprovider.RuntimeCapability{RequiredFlags: slices.Clone(wantFlags)},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, wantDigest, err := expected.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, gotDigest, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("runtime inspection digest=%s want=%s", gotDigest, wantDigest)
	}
	return gotDigest
}
