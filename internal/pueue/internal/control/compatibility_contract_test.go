package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestPrivateConfigBytesRemainCompatible(t *testing.T) {
	const base = "/state/.supervisor"
	got := privateConfigYAMLWithSocket(base, base+"/run/pueue.sock")
	want := `client:
  show_confirmation_questions: false
shared:
  pueue_directory: "/state/.supervisor/data"
  runtime_directory: "/state/.supervisor/run"
  alias_file: "/state/.supervisor/aliases.yml"
  use_unix_socket: true
  unix_socket_path: "/state/.supervisor/run/pueue.sock"
  unix_socket_permissions: 448
  pid_path: "/state/.supervisor/run/pueue.pid"
  shared_secret_path: "/state/.supervisor/data/shared_secret"
daemon:
  callback: "/state/.supervisor/delegate-terminal-event-v1 --terminal-event --root /state --pueue-id '{{id}}' --exit-code '{{exit_code}}'"
  shell_command:
    - "/bin/sh"
    - "-c"
    - "{{ pueue_command_string }}"
`
	if got != want {
		t.Fatalf("private config bytes changed:\n got: %q\nwant: %q", got, want)
	}
}

func TestPrivateMigrationRecordBytesAndErrorsRemainCompatible(t *testing.T) {
	record := compatibilityMigrationRecord()
	got, err := marshalPrivateMigrationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(
		`{"schema_version":1,"legacy":{"executable":"/usr/bin/pueued","sha256":"%s","config_path":"/state/.supervisor/pueue.yml","config_digest":"%s","resolved_config_sha256":"%s","endpoint":"unix:/state/.supervisor/run/pueue.sock"},"successor":{"executable":"/opt/pueued","sha256":"%s","config_path":"/state/.supervisor/pueue.yml","config_digest":"%s","resolved_config_sha256":"%s","endpoint":"unix:/state/.supervisor/run/pueue.sock"},"legacy_pid":4123,"legacy_process_start":"pid-start-4123","legacy_shutdown_requested":true,"legacy_shutdown_dispatched":true,"legacy_stopped":true}`+"\n",
		strings.Repeat("a", 64), strings.Repeat("c", 64), strings.Repeat("d", 64),
		strings.Repeat("b", 64), strings.Repeat("e", 64), strings.Repeat("f", 64),
	)
	if string(got) != want {
		t.Fatalf("migration record bytes changed:\n got: %q\nwant: %q", got, want)
	}

	identical := record
	identical.Successor = identical.Legacy
	_, err = marshalPrivateMigrationRecord(identical)
	if err == nil || !errors.Is(err, ErrConfiguration) || err.Error() != "invalid pueue configuration: private migration identities are identical" {
		t.Fatalf("identical identity error changed: %v", err)
	}

	invalidProcess := record
	invalidProcess.LegacyPID = -1
	_, err = marshalPrivateMigrationRecord(invalidProcess)
	if err == nil || !errors.Is(err, ErrConfiguration) || err.Error() != "invalid pueue configuration: invalid private migration predecessor state" {
		t.Fatalf("invalid process-state error changed: %v", err)
	}
}

func TestPrivateMigrationLoadKeepsUnsupportedSchemaError(t *testing.T) {
	base := t.TempDir()
	record := compatibilityMigrationRecord()
	record.SchemaVersion = task.SchemaVersion + 1
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(privateMigrationRecordPath(base), data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, found, err := loadPrivateMigrationRecord(base)
	want := fmt.Sprintf("%v: unsupported private migration journal schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	if found || err == nil || !errors.Is(err, task.ErrUnsupportedSchema) || errors.Is(err, ErrBinding) || err.Error() != want {
		t.Fatalf("unsupported-schema load contract changed: found=%t err=%v want=%q", found, err, want)
	}
}

func TestPrivateLockPathsModesAndWriterIntentRemainCompatible(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	ctx := context.Background()
	assertPrivateBootstrapAndTransitionLocks(t, ctx, stateRoot, base)

	digest := strings.Repeat("a", 64)
	client, err := NewClient(task.SupervisorRef{
		ClientExecutable:     "/usr/bin/pueue",
		ClientSHA256:         digest,
		ConfigPath:           filepath.Join(base, "pueue.yml"),
		ConfigDigest:         strings.Repeat("b", 64),
		ResolvedConfigSHA256: strings.Repeat("c", 64),
		Endpoint:             "unix:" + filepath.Join(base, "run", "pueue.sock"),
		ObservedVersion:      "4.0.4",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("close test client: %v", closeErr)
		}
	})

	assertPrivateQueueIdentityMutationContract(t, ctx, client, base)
	assertPrivateRecordPublicationContract(t, base)
}

func assertPrivateBootstrapAndTransitionLocks(t *testing.T, ctx context.Context, stateRoot, base string) {
	t.Helper()
	if err := WithBootstrapLock(ctx, stateRoot, func() error {
		assertPrivateLockHeld(t, ctx, filepath.Join(base, "bootstrap.lock"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertPrivateMode(t, base, 0o700)
	assertPrivateMode(t, filepath.Join(base, "bootstrap.lock"), 0o600)

	acquired, err := TryWithPrivateTransition(ctx, stateRoot, func() error {
		assertPrivateLockHeld(t, ctx, filepath.Join(base, privateTransitionLockName))
		return nil
	})
	if err != nil || !acquired {
		t.Fatalf("private transition lock contract changed: acquired=%t err=%v", acquired, err)
	}
	assertPrivateMode(t, filepath.Join(base, privateTransitionLockName), 0o600)
}

func assertPrivateQueueIdentityMutationContract(t *testing.T, ctx context.Context, client *Client, base string) {
	t.Helper()
	err := client.withPrivateQueueIdentityMutation(ctx, func(mutationContext context.Context) error {
		for _, name := range []string{
			privateQueueWriterIntentLockName,
			privateQueueIdentityGateLockName,
			privateQueueIdentityLockName,
		} {
			path := filepath.Join(base, name)
			assertPrivateMode(t, path, 0o600)
			assertPrivateLockHeld(t, mutationContext, path)
		}
		intent, readErr := os.ReadFile(filepath.Join(base, privateQueueWriterIntentLockName))
		if readErr != nil || string(intent) != privateQueueWriterWaitingState {
			t.Fatalf("writer intent must be committed before mutation callback: state=%q err=%v", intent, readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := os.ReadFile(filepath.Join(base, privateQueueWriterIntentLockName))
	if err != nil || string(intent) != privateQueueWriterIdleState {
		t.Fatalf("writer intent was not cleared after mutation: state=%q err=%v", intent, err)
	}
}

func assertPrivateRecordPublicationContract(t *testing.T, base string) {
	t.Helper()
	if privateRecordStagePrefix != ".private-record-" {
		t.Fatalf("private record stage prefix changed: %q", privateRecordStagePrefix)
	}
	const payload = "exact private record bytes\x00\xff"
	destination := filepath.Join(base, "compatibility.json")
	if err := publishPrivateRecord(base, destination, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile(destination)
	if err != nil || string(published) != payload {
		t.Fatalf("private record bytes changed: data=%q err=%v", published, err)
	}
	assertPrivateMode(t, destination, 0o600)
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), privateRecordStagePrefix) {
			t.Fatalf("committed private record left stage %q", entry.Name())
		}
	}
}

func compatibilityMigrationRecord() privateMigrationRecord {
	return privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy: privateDaemonIdentity{
			Executable:           "/usr/bin/pueued",
			SHA256:               strings.Repeat("a", 64),
			ConfigPath:           "/state/.supervisor/pueue.yml",
			ConfigDigest:         strings.Repeat("c", 64),
			ResolvedConfigSHA256: strings.Repeat("d", 64),
			Endpoint:             "unix:/state/.supervisor/run/pueue.sock",
		},
		Successor: privateDaemonIdentity{
			Executable:           "/opt/pueued",
			SHA256:               strings.Repeat("b", 64),
			ConfigPath:           "/state/.supervisor/pueue.yml",
			ConfigDigest:         strings.Repeat("e", 64),
			ResolvedConfigSHA256: strings.Repeat("f", 64),
			Endpoint:             "unix:/state/.supervisor/run/pueue.sock",
		},
		LegacyPID:                4123,
		LegacyProcessStart:       "pid-start-4123",
		LegacyShutdownRequested:  true,
		LegacyShutdownDispatched: true,
		LegacyStopped:            true,
	}
}

func assertPrivateMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %#o, want %#o", path, got, want)
	}
}

func assertPrivateLockHeld(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	lock, err := acquireExistingSupervisorLock(ctx, path, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, errSupervisorLockBusy) {
		return
	}
	if err != nil {
		t.Fatalf("probe lock %s: %v", path, err)
	}
	if closeErr := closeBootstrapLock(lock); closeErr != nil {
		t.Errorf("close unexpected acquired lock %s: %v", path, closeErr)
	}
	t.Fatalf("lock %s was not held at its callback boundary", path)
}
