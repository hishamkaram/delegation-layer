package control

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

func TestBindPrivateRejectsMalformedSupervisorBindingBeforeStart(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(privateSupervisorBindingPath(stateRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	options := Options{ObservationTimeout: 5 * time.Second}
	if _, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options); !errors.Is(err, ErrBinding) {
		t.Fatalf("bootstrap accepted malformed supervisor binding: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateBase, "ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started despite malformed supervisor binding: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateBase, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon launch marker exists after malformed supervisor binding: %v", err)
	}
}

func TestPrivateDaemonIdentityReplacementKeepsOtherRecordsCreateOnce(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	identityPath := privateDaemonIdentityPath(base)
	if err := publishPrivateRecord(base, identityPath, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateRecord(base, identityPath, []byte("second")); !errors.Is(err, ErrBinding) {
		t.Fatalf("create-once publisher accepted replacement: %v", err)
	}
	if err := replacePrivateDaemonIdentityRecord(base, []byte("second")); err != nil {
		t.Fatalf("deliberate record replacement failed: %v", err)
	}
	data, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("replacement record=%q, want second", data)
	}
}

func TestReplacePrivateSchedulingEvidenceReportsSchedulingContext(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	writePrivateStateRootRecord(t, stateRoot)
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := privateSchedulingEvidencePath(stateRoot)
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}

	err := ReplacePrivateSchedulingEvidence(stateRoot, []byte("evidence"))
	if err == nil {
		t.Fatal("scheduling evidence replacement unexpectedly succeeded over a directory")
	}
	if !errors.Is(err, ErrConfiguration) {
		t.Fatalf("scheduling evidence replacement error = %v, want ErrConfiguration", err)
	}
	if !strings.Contains(err.Error(), "private scheduling evidence") {
		t.Fatalf("scheduling evidence replacement error = %v, want scheduling evidence context", err)
	}
	if strings.Contains(err.Error(), "migration journal") {
		t.Fatalf("scheduling evidence replacement error used migration context: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(base, ".private-scheduling-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("scheduling evidence replacement leaked staged files: %v", stages)
	}
}

func TestStartDaemonRevalidatesIdentityBeforeStart(t *testing.T) {
	base := canonicalTemp(t)
	configPath := filepath.Join(base, "pueue.yml")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, daemonPath, privateDaemonFixture)
	originalDigest, err := hashExecutable(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonPath, []byte(privateDaemonFixture+"\nchanged\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := startDaemon(daemonPath, originalDigest, configPath, base, Options{}); !errors.Is(err, ErrBinding) {
		t.Fatalf("changed daemon identity was started: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon started after pre-start identity drift: %v", err)
	}
}

func TestRecoverPrivateRejectsDeletedConfigBeforeRestart(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: 5 * time.Second}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	configPath := first.Binding().ConfigPath
	if err = os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if _, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), clientPath, daemonPath, options); !errors.Is(err, ErrBinding) {
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

func TestBindPrivateSerializesConcurrentBootstrap(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: privateTestObservationTimeout}
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

func waitForPrivateFixtureMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect private status start marker: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("private status fixture did not reach its blocking point")
}

func releasePrivateStatus(t *testing.T, marker string) {
	t.Helper()
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func releasePrivateStatusOnCleanup(t *testing.T, marker string) {
	t.Helper()
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Errorf("release private status fixture: %v", err)
	}
}
