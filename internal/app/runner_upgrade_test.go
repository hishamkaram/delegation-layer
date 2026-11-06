package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestPrepareAdmissionExecutablesKeepsExplicitRunner(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	custom := filepath.Join(supervisorfixture.CanonicalTempDir(t), "custom-delegate-run")
	writeRunnerFixture(t, custom, "custom runner")
	got, err := prepareAdmissionExecutables(Arguments{
		Runner:      custom,
		PueueConfig: filepath.Join(supervisorfixture.CanonicalTempDir(t), "external-pueue.yml"),
	}, Dependencies{ResourceChecker: fixtureResources}, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Fatalf("explicit runner changed to %q, want %q", got, custom)
	}
	if _, err = os.Lstat(filepath.Join(root, ".supervisor")); !os.IsNotExist(err) {
		t.Fatalf("custom runner created a state supervisor directory: %v", err)
	}
}

func TestPrepareAdmissionExecutablesRejectsPrivateConfigBeforeInstallingState(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	_, err := prepareAdmissionExecutables(Arguments{PueueConfig: pueue.PrivateConfigPath(root)}, Dependencies{ResourceChecker: fixtureResources}, root)
	if !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("private config collision error = %v, want configuration error", err)
	}
	if _, err = os.Lstat(filepath.Join(root, ".supervisor")); !os.IsNotExist(err) {
		t.Fatalf("rejected config collision left supervisor state: %v", err)
	}
}

func TestPrepareAdmissionExecutablesUsesSavedPrivateSupervisorPair(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	runnerSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, runnerSource, "managed runner")
	delegate := filepath.Join(supervisorfixture.CanonicalTempDir(t), "bin", "delegate")
	writeRunnerFixture(t, delegate, "delegate")
	client, _, _, _ := newOrdinarySubmissionReceiptSupervisor(t)
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	saved := client.Binding()
	saved.DaemonExecutable = filepath.Join(filepath.Dir(saved.ClientExecutable), "pueued")
	writeRunnerFixture(t, saved.DaemonExecutable, "#!/bin/sh\nexit 0\n")
	saved.ConfigPath = pueue.PrivateConfigPath(root)

	got, err := prepareAdmissionExecutables(Arguments{savedSupervisor: &saved}, Dependencies{
		ResourceChecker:             fixtureResources,
		InitialSupervisorExecutable: delegate,
		RunnerExecutable:            runnerSource,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != runnerSource {
		t.Fatalf("managed runner=%q, want installed %q", got, runnerSource)
	}
	if _, err = os.Lstat(filepath.Join(root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selection wrote supervisor state: %v", err)
	}
}
