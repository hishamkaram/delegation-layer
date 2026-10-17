package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func requireSupervisorPairGenerationChanged(t *testing.T, firstClient, firstDaemon, secondClient, secondDaemon string) {
	t.Helper()
	if firstClient == secondClient || firstDaemon == secondDaemon {
		t.Fatal("supervisor pair upgrade reused the old generation")
	}
}

func requireResolvedSupervisorPair(t *testing.T, base, wantClient, wantDaemon string) (string, string) {
	t.Helper()
	client, daemon, found, err := supervisor.ResolveStateSupervisorPair(base)
	if err != nil || !found {
		t.Fatalf("state-root pair resolution found=%t err=%v", found, err)
	}
	for _, executable := range []struct {
		path string
		want string
	}{{path: client, want: wantClient}, {path: daemon, want: wantDaemon}} {
		got, readErr := os.ReadFile(executable.path)
		if readErr != nil || !strings.HasSuffix(string(got), executable.want) {
			t.Fatalf("resolved state-root executable %q = %q, err=%v", executable.path, got, readErr)
		}
	}
	return client, daemon
}

func TestPrivateRecoveryUsesStateRootPairWhenInstallPairsAreUnavailable(t *testing.T) {
	root := filepath.Join(testsupport.CanonicalTempDir(t), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	saved.ConfigPath = pueue.PrivateConfigPath(root)
	saved.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("resolved config"))
	saved.Endpoint = "unix:" + filepath.Join(root, ".supervisor", "runtime", "pueue.sock")
	saved.ConfigDigest = task.ComputeSHA256([]byte("config"))
	saved.ObservedVersion = "pueue 4.0.4"
	if err := supervisor.PrepareStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", testsupport.CanonicalTempDir(t))

	missingDelegate := filepath.Join(testsupport.CanonicalTempDir(t), "missing", "delegate")
	_, err := newSupervisorClient(context.Background(), root, saved, pueue.Options{}, true, missingDelegate)
	if err == nil || !strings.Contains(err.Error(), "saved private supervisor config is unavailable") {
		t.Fatalf("recovery did not reach the saved private config check using the state-root pair: %v", err)
	}
}

func TestPrivateRecoveryRejectsTamperedStateRootSupervisorPair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, path, external string)
	}{
		{
			name: "changed executable bytes",
			tamper: func(t *testing.T, path, _ string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("replaced client"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked executable",
			tamper: func(t *testing.T, path, external string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testsupport.CanonicalTempDir(t)
			clientSource, daemonSource := writeSavedSupervisorPair(t)
			if err := supervisor.PrepareStateSupervisorPair(context.Background(), root, clientSource, daemonSource); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(root, ".supervisor")
			client, daemon, found, err := supervisor.ResolveStateSupervisorPair(base)
			if err != nil || !found {
				t.Fatalf("resolve installed state-root pair: found=%t err=%v", found, err)
			}
			saved := savedSupervisorPairRef(t, client, daemon)
			saved.ConfigPath = pueue.PrivateConfigPath(root)
			tc.tamper(t, client, clientSource)

			if _, err = newSupervisorClient(context.Background(), root, saved, pueue.Options{}, true, filepath.Join(testsupport.CanonicalTempDir(t), "delegate")); !errors.Is(err, pueue.ErrBinding) {
				t.Fatalf("tampered state-root supervisor pair error = %v, want binding error", err)
			}
		})
	}
}

func writeSavedSupervisorPair(t *testing.T) (string, string) {
	t.Helper()
	client := filepath.Join(testsupport.CanonicalTempDir(t), "pueue")
	daemon := filepath.Join(testsupport.CanonicalTempDir(t), "pueued")
	writeRunnerFixture(t, client, "saved client")
	writeRunnerFixture(t, daemon, "saved daemon")
	return client, daemon
}

func savedSupervisorPairRef(t *testing.T, client, daemon string) task.SupervisorRef {
	t.Helper()
	clientDigest, err := commonprovider.FingerprintExecutable(client)
	if err != nil {
		t.Fatal(err)
	}
	daemonDigest, err := commonprovider.FingerprintExecutable(daemon)
	if err != nil {
		t.Fatal(err)
	}
	return task.SupervisorRef{
		ClientExecutable: client, ClientSHA256: clientDigest,
		DaemonExecutable: daemon, DaemonSHA256: daemonDigest,
	}
}
