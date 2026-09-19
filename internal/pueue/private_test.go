package pueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadyRequiresTheBoundedQueueSchema(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	if err := os.WriteFile(fake.statusPath, []byte(`{"tasks":{},"groups":{},"future":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fake.client.Ready(context.Background()); !errors.Is(err, ErrBinding) {
		t.Fatalf("malformed supervisor status was accepted: %v", err)
	}
}

func TestWaitReadyJoinsTimedOutStatusCommand(t *testing.T) {
	fake := newFakeSupervisor(t, "delay-status")
	result := make(chan error, 1)
	go func() {
		result <- waitReady(context.Background(), fake.client, 20*time.Millisecond)
	}()
	select {
	case err := <-result:
		t.Fatalf("waitReady returned before its status command was reaped: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := <-result; err == nil {
		t.Fatal("waitReady accepted an expired readiness observation")
	}
}

func TestBindPrivateJoinsTimedOutVersionProbeBeforeReturning(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	marker := filepath.Join(base, "version-marker")
	options := Options{
		ObservationTimeout: 20 * time.Millisecond,
		Environment: append(os.Environ(),
			"PRIVATE_DELAY_VERSION=1",
			"PRIVATE_VERSION_MARKER="+marker,
		),
	}
	if _, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options); err == nil {
		t.Fatal("timed out version probe unexpectedly bootstrapped the supervisor")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "started\ncompleted\n" {
		t.Fatalf("version probe was not reaped before return: %q", data)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after a failed version probe: %v", err)
	}
}

func TestBindPrivateUsesLateSuccessfulReadinessWithoutStartingDaemon(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateBase, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	seedPrivateDaemonIdentity(t, privateBase, daemonPath)
	options := Options{
		ObservationTimeout: 20 * time.Millisecond,
		Environment:        append(os.Environ(), "PRIVATE_DELAY_STATUS=1"),
	}
	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("late successful readiness returned no client")
	}
	if _, err := os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started despite a successful late readiness result: %v", err)
	}
}

func TestBindPrivateDoesNotRestartOnMalformedReadyResponse(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateBase, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	seedPrivateDaemonIdentity(t, privateBase, daemonPath)
	options := Options{
		ObservationTimeout: time.Second,
		Environment:        append(os.Environ(), "PRIVATE_INVALID_STATUS=1"),
	}
	if _, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options); !errors.Is(err, ErrBinding) {
		t.Fatalf("malformed ready response was treated as daemon absence: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after malformed ready response: %v", err)
	}
}

func TestBindPrivateRejectsReadyDaemonIdentityDrift(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateBase, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	seedPrivateDaemonIdentity(t, privateBase, daemonPath)
	file, err := os.OpenFile(daemonPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: time.Second}); !errors.Is(err, ErrBinding) {
		t.Fatalf("ready daemon identity drift was accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after ready identity drift: %v", err)
	}
}

func TestCreatePrivateConfigPublishesCompleteFile(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "pueue.yml")
	want := "private-config\n"
	if err := createPrivateConfig(configPath, base, want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("published private config=%q, want %q", got, want)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pueue.yml-") {
			t.Fatalf("staged private config was left behind: %s", entry.Name())
		}
	}
}

func TestBindPrivateBootstrapsAndReusesSupervisor(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Binding().ObservedVersion; got != "pueue 99.7.3" {
		t.Fatalf("runtime version was not preserved: %q", got)
	}
	if first.Binding().DaemonExecutable != daemonPath || first.Binding().DaemonSHA256 == "" {
		t.Fatalf("private daemon identity was not persisted: %+v", first.Binding())
	}
	configPath := filepath.Join(stateRoot, privateSupervisorDirectory, "pueue.yml")
	if _, statErr := os.Stat(configPath); statErr != nil {
		t.Fatalf("private config was not created: %v", statErr)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil || !strings.Contains(string(data), filepath.Join(stateRoot, privateSupervisorDirectory, "data")) {
		t.Fatalf("private config is not rooted in state: data=%q err=%v", data, readErr)
	}

	second, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if second.Binding().ConfigPath != first.Binding().ConfigPath || second.Binding().Endpoint != first.Binding().Endpoint {
		t.Fatalf("private binding changed across reuse: first=%+v second=%+v", first.Binding(), second.Binding())
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("private daemon was started %d times, want one: %q", got, log)
	}
}

func TestRecoverPrivateRestartsStoppedSupervisor(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverPrivate(context.Background(), stateRoot, first.Binding(), options)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Binding() != first.Binding() {
		t.Fatalf("recovery changed the saved supervisor binding: first=%+v recovered=%+v", first.Binding(), recovered.Binding())
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 2 {
		t.Fatalf("private daemon was restarted %d times, want 2: %q", got, log)
	}
}

func TestRecoverPrivateRejectsDaemonIdentityDriftBeforeRestart(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(daemonPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), options); !errors.Is(err, ErrBinding) {
		t.Fatalf("recovery accepted daemon identity drift: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("daemon restarted after identity drift %d times, want 1: %q", got, log)
	}
}

func TestRecoverPrivateRejectsDeletedConfigBeforeRestart(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	configPath := first.Binding().ConfigPath
	if err = os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), options); !errors.Is(err, ErrBinding) {
		t.Fatalf("recovery accepted a deleted saved config: %v", err)
	}
	if _, err = os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery recreated a deleted saved config: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("private daemon restarted after stale binding rejection %d times, want 1: %q", got, log)
	}
}

func TestJoinPendingWithinLeavesUnfinishedCommandOwned(t *testing.T) {
	pending := &Pending{done: make(chan struct{})}
	started := time.Now()
	if joinPendingWithin(pending, 20*time.Millisecond) {
		t.Fatal("unfinished pending command was reported complete")
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("pending grace was not bounded near its deadline: %s", elapsed)
	}
	close(pending.done)
	if !joinPendingWithin(pending, time.Second) {
		t.Fatal("completed pending command was not observed")
	}
}

func TestBindPrivateSerializesConcurrentBootstrap(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: time.Second}
	clients := make(chan *Client, 2)
	errorsFound := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
			if err != nil {
				errorsFound <- err
				return
			}
			clients <- client
		}()
	}
	group.Wait()
	close(clients)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if got := len(clients); got != 2 {
		t.Fatalf("concurrent private binds returned %d clients, want 2", got)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 1 {
		t.Fatalf("private daemon was started %d times, want one: %q", got, log)
	}
}

func writeExecutable(t *testing.T, path, source string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
}

func seedPrivateDaemonIdentity(t *testing.T, base, daemonPath string) {
	t.Helper()
	resolved, err := canonicalExecutable(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hashExecutable(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateDaemonIdentity(base, task.SupervisorRef{DaemonExecutable: resolved, DaemonSHA256: digest}); err != nil {
		t.Fatal(err)
	}
}

const privateClientFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    --version)
      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' started >> "$PRIVATE_VERSION_MARKER"; fi
      if [ "${PRIVATE_DELAY_VERSION:-}" = "1" ]; then sleep 0.08; fi
      if [ "${PRIVATE_VERSION_MARKER:-}" != "" ]; then printf '%s\n' completed >> "$PRIVATE_VERSION_MARKER"; fi
      printf '%s\n' 'pueue 99.7.3'; exit 0 ;;
    status)
      if [ "${PRIVATE_DELAY_STATUS:-}" = "1" ]; then sleep 0.08; fi
      marker="$(dirname "$config")/ready"
      [ -f "$marker" ] || exit 1
      if [ "${PRIVATE_INVALID_STATUS:-}" = "1" ]; then
        printf '%s\n' '{"tasks":{}}'
        exit 0
      fi
      printf '%s\n' '{"tasks":{},"groups":{}}'
      exit 0
      ;;
    *) exit 64 ;;
  esac
done
exit 64
`

const privateDaemonFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' started >> "$(dirname "$config")/daemon-started"
touch "$(dirname "$config")/ready"
`
