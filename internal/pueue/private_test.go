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

func TestBindPrivateBootstrapsAndReusesSupervisor(t *testing.T) {
	base := filepath.Clean(t.TempDir())
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

func TestBindPrivateSerializesConcurrentBootstrap(t *testing.T) {
	base := filepath.Clean(t.TempDir())
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

const privateClientFixture = `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    --version) printf '%s\n' 'pueue 99.7.3'; exit 0 ;;
    status)
      marker="$(dirname "$config")/ready"
      [ -f "$marker" ] || exit 1
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
