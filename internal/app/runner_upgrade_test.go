package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func installStateRunnerFixture(t *testing.T, root, source string) string {
	t.Helper()
	runner, err := supervisor.InstallStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func requireRunnerFixture(t *testing.T, runner, contents string) {
	t.Helper()
	supervisorfixture.RequireExecutable(t, runner, contents)
}

func TestPrepareAdmissionExecutablesKeepsExplicitRunner(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	custom := filepath.Join(supervisorfixture.CanonicalTempDir(t), "custom-delegate-run")
	writeRunnerFixture(t, custom, "custom runner")
	got, err := prepareAdmissionExecutables(Arguments{
		Runner:      custom,
		PueueConfig: filepath.Join(supervisorfixture.CanonicalTempDir(t), "external-pueue.yml"),
	}, Dependencies{}, root)
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
	_, err := prepareAdmissionExecutables(Arguments{PueueConfig: pueue.PrivateConfigPath(root)}, Dependencies{}, root)
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
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	saved.ConfigPath = pueue.PrivateConfigPath(root)

	got, err := prepareAdmissionExecutables(Arguments{savedSupervisor: &saved}, Dependencies{
		InitialSupervisorExecutable: delegate,
		RunnerExecutable:            runnerSource,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !supervisor.IsStateRunnerPath(root, got) {
		t.Fatalf("managed runner path = %q, want state-root runner", got)
	}
	requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "saved client", "saved daemon")
}

func TestPrepareAdmissionExecutablesUsesStateRootPairWhenInstallPairsAreUnavailable(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	saved.ConfigPath = pueue.PrivateConfigPath(root)
	if err := supervisor.PrepareStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", supervisorfixture.CanonicalTempDir(t))

	runner := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, runner, "managed runner")
	missingDelegate := filepath.Join(supervisorfixture.CanonicalTempDir(t), "missing", "delegate")
	got, err := prepareAdmissionExecutables(Arguments{savedSupervisor: &saved}, Dependencies{
		InitialSupervisorExecutable: missingDelegate,
		RunnerExecutable:            runner,
	}, root)
	if err != nil {
		t.Fatalf("admission could not reuse the verified state-root supervisor pair: %v", err)
	}
	if !supervisor.IsStateRunnerPath(root, got) {
		t.Fatalf("admission runner = %q, want managed state-root runner", got)
	}
	requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "saved client", "saved daemon")
}

func TestBindInitialUsesStateRootPairWhenBundleIsUnavailable(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	sourceDirectory := supervisorfixture.CanonicalTempDir(t)
	clientSource := filepath.Join(sourceDirectory, "pueue")
	daemonSource := filepath.Join(sourceDirectory, "pueued")
	writeRunnerFixture(t, clientSource, `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|--config) config=$2; shift 2 ;;
    --version) printf '%s\n' 'pueue 99.7.3'; exit 0 ;;
    parallel) [ "$2" = 0 ]; exit 0 ;;
    status)
      base=${config%/*}
      [ -f "$base/ready" ] || exit 1
      printf '%s\n' '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}'
      exit 0 ;;
    *) shift ;;
  esac
done
exit 64
`)
	writeRunnerFixture(t, daemonSource, `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|--config) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
base=${config%/*}
: > "$base/ready"
`)
	if err := supervisor.PrepareStateSupervisorPair(context.Background(), root, clientSource, daemonSource); err != nil {
		t.Fatal(err)
	}
	clientPath, daemonPath, found, err := supervisor.ResolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("state-root pair was not installed: found=%v err=%v", found, err)
	}
	if err = os.Remove(clientSource); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(daemonSource); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", supervisorfixture.CanonicalTempDir(t))
	missingDelegate := filepath.Join(supervisorfixture.CanonicalTempDir(t), "missing", "delegate")
	// This verifies executable-pair resolution, so use the production window;
	// race instrumentation must not turn scheduler delay into a timeout test.
	client, err := supervisor.BindInitialWithStart(supervisorAdmissionSelection(Arguments{}), supervisorAdmissionOptions(Dependencies{InitialSupervisorExecutable: missingDelegate}, pueue.Options{ObservationTimeout: pueue.DefaultObservationTimeout}), root, true)
	if err != nil {
		t.Fatalf("private supervisor did not bind from its verified state-root pair: %v", err)
	}
	if binding := client.Binding(); binding.ClientExecutable != clientPath || binding.DaemonExecutable != daemonPath {
		t.Fatalf("private binding used a different executable pair: got=(%q,%q) want=(%q,%q)", binding.ClientExecutable, binding.DaemonExecutable, clientPath, daemonPath)
	}
}
