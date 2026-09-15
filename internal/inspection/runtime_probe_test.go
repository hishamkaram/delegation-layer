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

func TestRuntimeCapabilityRejectsMissingRequiredFlag(t *testing.T) {
	root := t.TempDir()
	path := writeRuntimeCLI(t, root, "provider 2.0.0", "--sandbox")
	definition := runtimeDefinition(t, root, path, []string{"--sandbox", "--missing"})
	if _, err := runRuntimeProbe(t, definition); err == nil {
		t.Fatal("missing required flag was accepted")
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
