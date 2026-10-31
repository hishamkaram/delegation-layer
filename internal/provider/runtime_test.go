package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRuntimeVersionArgsAreCopiedAndBoundToTheCapabilityDigest(t *testing.T) {
	definition, err := NewRuntimeInspectionDefinition(CLIInfo{Path: "/usr/bin/provider", SHA256: strings.Repeat("a", 64)}, "/workspace", []string{"LANG=C"}, RuntimeCapability{VersionArgs: []string{"version"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(definition.Runtime.VersionArgs, []string{"version"}) {
		t.Fatalf("version args=%q", definition.Runtime.VersionArgs)
	}
	changed := definition
	runtime := *definition.Runtime
	runtime.VersionArgs = []string{"--version"}
	changed.Runtime = &runtime
	oldDigest, err := RuntimeCapabilityDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := RuntimeCapabilityDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if oldDigest == newDigest {
		t.Fatal("version command drift was omitted from capability digest")
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

// TestLocateCLIPathResolvesWithoutReadingProviderBytes replaces the retired
// fingerprinting locator: discovery checks availability and records only the
// canonical path, so an unreadable executable is still a usable provider.
func TestLocateCLIPathResolvesWithoutReadingProviderBytes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "provider")
	if err = os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o100); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err = os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{path, alias} {
		info, locateErr := LocateCLIPath(input)
		if locateErr != nil || info != (CLIInfo{Path: path}) {
			t.Fatalf("LocateCLIPath(%q) = %+v, %v; want canonical path only", input, info, locateErr)
		}
	}
	if _, err = LocateCLIPath(root); err == nil {
		t.Fatal("directory was accepted as a provider executable")
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
