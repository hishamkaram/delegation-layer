package inspection

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
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type runtimeTestStopper struct{}

func (runtimeTestStopper) PrepareBudget(time.Time) (execution.BudgetRequest, error) {
	return func(context.Context) error { return nil }, nil
}

func TestRuntimeCapabilityAcceptsArbitraryVersionWhenFlagsExist(t *testing.T) {
	root := t.TempDir()
	path := writeRuntimeCLI(t, root, "provider 99.42.7 (nightly)", "--sandbox --output")
	definition := runtimeDefinition(t, root, path, []string{"--sandbox", "--output"})
	facts, err := runRuntimeProbe(t, definition)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Version != "provider 99.42.7 (nightly)" || facts.Executable != definition.Executable {
		t.Fatalf("unexpected runtime facts: %+v", facts)
	}
}

func TestRuntimeCapabilityRejectsVersionThatCannotFitDurableEvidence(t *testing.T) {
	root := t.TempDir()
	path := writeRuntimeCLI(t, root, strings.Repeat("v", task.MaxProviderRuntimeEvidenceVersionBytes+1), "--sandbox")
	definition := runtimeDefinition(t, root, path, []string{"--sandbox"})
	if _, err := runRuntimeProbe(t, definition); err == nil || !strings.Contains(err.Error(), "invalid-runtime-facts") {
		t.Fatalf("unrecordable runtime version result=%v, want invalid-runtime-facts", err)
	}
}

func TestRuntimeCapabilityDigestSeparatesContractFromExecutableIdentity(t *testing.T) {
	root := t.TempDir()
	firstPath := writeRuntimeCLI(t, root, "provider 1.0.0", "--sandbox --output")
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
	path := writeRuntimeCLI(t, root, "provider 2.0.0", "--sandbox")
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

func writeRuntimeCLI(t *testing.T, directory, version, help string) string {
	t.Helper()
	path := filepath.Join(directory, "provider-cli")
	script := "#!/bin/sh\nset -eu\ncase \"${1-}\" in\n  --version) printf '%s\\n' '" + version + "' ;;\n  --help) printf '%s\\n' '" + help + "' ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(version+help, "\x00\n") {
		t.Fatal("test CLI inputs contain unsupported control characters")
	}
	return path
}
