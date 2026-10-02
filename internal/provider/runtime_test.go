package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestParseCLIVersionAcceptsChangedReportedVersion(t *testing.T) {
	for _, output := range []string{"provider 99.42.7 (nightly)\n", "v2026.09\n", "custom build"} {
		got, err := ParseCLIVersion([]byte(output))
		if err != nil || got != strings.TrimSpace(output) {
			t.Fatalf("version %q: got %q err=%v", output, got, err)
		}
	}
}

func TestParseCLIVersionRejectsInvalidOutput(t *testing.T) {
	for _, output := range [][]byte{nil, []byte("  \n"), []byte("provider\x00version\n"), {0xff}} {
		if _, err := ParseCLIVersion(output); err == nil {
			t.Fatalf("invalid version output %q was accepted", output)
		}
	}
}

func TestEncodeInspectionFactsAcceptsLargeRuntimeVersion(t *testing.T) {
	facts := RuntimeFacts{
		Executable: "/usr/bin/provider",
		Version:    strings.Repeat("v", 8<<10),
		SHA256:     strings.Repeat("a", 64),
	}
	if _, err := EncodeInspectionFacts(facts, nil); err != nil {
		t.Fatalf("large runtime version was rejected: %v", err)
	}
}

func TestEncodeInspectionFactsRejectsUnrecordableRuntimeVersion(t *testing.T) {
	facts := RuntimeFacts{
		Executable: "/usr/bin/provider",
		Version:    strings.Repeat("v", task.MaxProviderRuntimeEvidenceVersionBytes+1),
		SHA256:     strings.Repeat("a", 64),
	}
	if _, err := EncodeInspectionFacts(facts, nil); err == nil {
		t.Fatal("runtime version exceeding durable evidence bound was accepted")
	}
}

func TestContainsCLIFlagRequiresCompleteToken(t *testing.T) {
	if !ContainsCLIFlag([]byte("  --sandbox   --output-last-message\n"), "--sandbox") {
		t.Fatal("advertised flag was not found")
	}
	for _, output := range []string{"--sandboxed", "prefix--sandbox", "--sandbox_value"} {
		if ContainsCLIFlag([]byte(output), "--sandbox") {
			t.Fatalf("partial flag %q was accepted", output)
		}
	}
}

func TestLocateCLIRejectsMissingAndNonExecutable(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	if _, err := LocateCLIPath(missing); err == nil {
		t.Fatal("missing executable was accepted")
	}
	nonExecutable := filepath.Join(root, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LocateCLIPath(nonExecutable); err == nil {
		t.Fatal("non-executable file was accepted")
	}
}

func TestLocateCLIPathFingerprintsRegularExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' version\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := LocateCLIPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path == "" || info.SHA256 == "" || info.Version != "" {
		t.Fatalf("unexpected static executable info: %+v", info)
	}
}

func TestRuntimeInspectionContractDigestExcludesOnlyRuntimeIdentity(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "provider-old")
	newPath := filepath.Join(t.TempDir(), "provider-new")
	oldSHA := task.ComputeSHA256([]byte("old"))
	newSHA := task.ComputeSHA256([]byte("new"))
	definition, err := NewRuntimeInspectionDefinition(CLIInfo{Path: oldPath, SHA256: oldSHA}, "/workspace", []string{"LANG=C"}, RuntimeCapability{
		HelpArgs: []string{"exec"}, RequiredFlags: []string{"--json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition.Executable = "/usr/bin/security"
	definition.ExecutableSHA256 = task.ComputeSHA256([]byte("security"))
	definition.Arguments = []string{"--read-policy"}
	definition.Project = func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"ok":true}`), nil }
	definition, _, err = definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	changed := definition
	runtime := *definition.Runtime
	runtime.Executable = newPath
	runtime.ExecutableSHA256 = newSHA
	changed.Runtime = &runtime
	oldDigest, err := RuntimeInspectionContractDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := RuntimeInspectionContractDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if oldDigest != newDigest {
		t.Fatalf("runtime identity changed the mixed inspection contract: old=%s new=%s", oldDigest, newDigest)
	}
	changed.Arguments = []string{"--different-policy"}
	changed, _, err = changed.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	changedDigest, err := RuntimeInspectionContractDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == oldDigest {
		t.Fatal("mixed native inspection contract drift was ignored")
	}
	providerMixed := definition
	providerMixed.Executable = oldPath
	providerMixed.ExecutableSHA256 = oldSHA
	providerMixed.Runtime = &RuntimeProbeDefinition{
		Executable: oldPath, ExecutableSHA256: oldSHA, Directory: definition.Directory,
		Environment: append([]string(nil), definition.Environment...), HelpArgs: []string{"exec"}, RequiredFlags: []string{"--json"},
	}
	providerMixed, _, err = providerMixed.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	providerChanged := providerMixed
	providerRuntime := *providerMixed.Runtime
	providerRuntime.Executable = newPath
	providerRuntime.ExecutableSHA256 = newSHA
	providerChanged.Runtime = &providerRuntime
	providerChanged.Executable = newPath
	providerChanged.ExecutableSHA256 = newSHA
	providerOldDigest, err := RuntimeInspectionContractDigest(providerMixed)
	if err != nil {
		t.Fatal(err)
	}
	providerNewDigest, err := RuntimeInspectionContractDigest(providerChanged)
	if err != nil {
		t.Fatal(err)
	}
	if providerOldDigest != providerNewDigest {
		t.Fatal("provider-owned executable identity changed the mixed inspection contract")
	}
}
