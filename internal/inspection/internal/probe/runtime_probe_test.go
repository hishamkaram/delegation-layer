package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

type runtimeTestStopper struct{}

func (runtimeTestStopper) PrepareBudget(time.Time) (execution.BudgetRequest, error) {
	return func(context.Context) error { return nil }, nil
}

func TestRuntimeCapabilityNeverRunsVersionCommand(t *testing.T) {
	root := t.TempDir()
	path := writeRuntimeCLI(t, root, "--sandbox --output")
	definition := runtimeDefinition(t, root, path, []string{"--sandbox", "--output"})
	facts, err := runRuntimeProbe(t, definition)
	if err != nil {
		t.Fatalf("usable CLI without a version command was refused: %v", err)
	}
	if facts != (provider.RuntimeFacts{Executable: definition.Executable}) {
		t.Fatalf("runtime facts = %+v, want path-only observation", facts)
	}
	if invoked := runtimeCLIInvocations(t, root); invoked != "--help\n" {
		t.Fatalf("provider invocations = %q, want only the required help exchange", invoked)
	}
}

func TestRuntimeCapabilityRejectsRequiredHelpOverflow(t *testing.T) {
	root := t.TempDir()
	path := writeRuntimeCLI(t, root, "--sandbox "+strings.Repeat("x", 64))
	definition := runtimeDefinition(t, root, path, []string{"--sandbox"})
	var probeErr error
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{
		Stopper: runtimeTestStopper{},
	}, func(scope execution.PreflightScope) error {
		_, probeErr = InspectRuntime(scope, *definition.Runtime, 16)
		return probeErr
	})
	if probeErr == nil || !strings.Contains(probeErr.Error(), "help-command") || errors.Join(workErr, stopErr) == nil {
		t.Fatalf("required help overflow result=%v work=%v stop=%v, want help-command failure", probeErr, workErr, stopErr)
	}
}

func TestRuntimeCapabilityRunsReplacedInstalledExecutable(t *testing.T) {
	root := t.TempDir()
	path := writeRuntimeCLI(t, root, "--sandbox")
	definition := runtimeDefinition(t, root, path, []string{"--sandbox"})
	// A compatible provider update replaces the installed file after
	// discovery. The probe runs the installed executable; no byte gate refuses.
	updated := filepath.Join(root, "provider-cli.updated")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"updated:$*\" >>" + filepath.Join(root, "invocations") + "\ncase \"${1-}\" in\n  --help) printf '%s\\n' '--sandbox --new-flag' ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(updated, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(updated, path); err != nil {
		t.Fatal(err)
	}
	facts, err := runRuntimeProbe(t, definition)
	if err != nil || facts.Executable != path || facts.SHA256 != "" {
		t.Fatalf("compatible update refused: facts=%+v err=%v", facts, err)
	}
	if invoked := runtimeCLIInvocations(t, root); invoked != "updated:--help\n" {
		t.Fatalf("provider invocations = %q, want the installed update's help exchange", invoked)
	}
}

func TestRuntimeCapabilityDigestSeparatesContractFromExecutableIdentity(t *testing.T) {
	root := t.TempDir()
	firstPath := writeRuntimeCLI(t, root, "--sandbox --output")
	secondPath := filepath.Join(root, "provider-cli-next")
	data, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(secondPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	first := runtimeDefinition(t, root, firstPath, []string{"--sandbox", "--output"})
	second := runtimeDefinition(t, root, secondPath, []string{"--sandbox", "--output"})
	firstDigest, err := provider.RuntimeCapabilityDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := provider.RuntimeCapabilityDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatal("executable identity changed the capability contract digest")
	}
	changed := runtimeDefinition(t, root, secondPath, []string{"--sandbox"})
	changedDigest, err := provider.RuntimeCapabilityDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("required flag change was omitted from the capability contract digest")
	}
}

func TestRuntimeCapabilityRejectsMissingRequiredFlag(t *testing.T) {
	root := t.TempDir()
	path := writeRuntimeCLI(t, root, "--sandbox")
	definition := runtimeDefinition(t, root, path, []string{"--sandbox", "--missing"})
	if _, err := runRuntimeProbe(t, definition); err == nil {
		t.Fatal("missing required flag was accepted")
	} else if !strings.Contains(err.Error(), "missing-required-flag---missing") {
		t.Fatalf("missing capability diagnostic = %v", err)
	}
}

func runRuntimeProbe(t *testing.T, definition provider.InspectionDefinition) (provider.RuntimeFacts, error) {
	t.Helper()
	var facts provider.RuntimeFacts
	var probeErr error
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{
		Stopper: runtimeTestStopper{},
	}, func(scope execution.PreflightScope) error {
		facts, probeErr = InspectRuntime(scope, *definition.Runtime, definition.OutputLimit)
		return probeErr
	})
	return facts, errors.Join(workErr, stopErr)
}

func runtimeDefinition(t *testing.T, directory, path string, requiredFlags []string) provider.InspectionDefinition {
	t.Helper()
	info, err := provider.LocateCLIPath(path)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewRuntimeInspectionDefinition(info, directory, []string{}, provider.RuntimeCapability{RequiredFlags: requiredFlags})
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

// writeRuntimeCLI installs a provider fixture that answers --help, logs every
// invocation, and rejects every other command, including --version.
func writeRuntimeCLI(t *testing.T, directory, help string) string {
	t.Helper()
	if strings.ContainsAny(help, "\x00\n'") {
		t.Fatal("test CLI help contains unsupported characters")
	}
	path := filepath.Join(directory, "provider-cli")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >>" + filepath.Join(directory, "invocations") + "\ncase \"${1-}\" in\n  --help) printf '%s\\n' '" + help + "' ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func runtimeCLIInvocations(t *testing.T, directory string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
