package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

const (
	snapshotMarkerKey      = "LANG"
	snapshotInheritedValue = "issue91-inherited-marker"
	snapshotSuppliedValue  = "issue91-supplied-marker"
	// The fake client reports only the synthetic marker variable it received.
	snapshotClientScript = "#!/bin/sh\nprintf 'pueue marker=%s\\n' \"${" + snapshotMarkerKey + "-unset}\"\n"
)

func TestOptionsForEnvironmentWithProcessScopeReachesClientChild(t *testing.T) {
	t.Setenv(snapshotMarkerKey, snapshotInheritedValue)
	for _, tc := range []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "nil keeps bounded default", environment: nil, want: snapshotInheritedValue},
		{name: "explicit empty stays empty", environment: []string{}, want: "unset"},
		{name: "supplied values pass through", environment: []string{snapshotMarkerKey + "=" + snapshotSuppliedValue}, want: snapshotSuppliedValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := OptionsForEnvironment(pueue.Options{Environment: tc.environment}, os.Environ())
			if got := observeSnapshotMarker(t, options); got != tc.want {
				t.Fatalf("client child observed marker %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOptionsForEnvironmentWithProcessScopeOwnsItsCopy(t *testing.T) {
	supplied := []string{snapshotMarkerKey + "=" + snapshotSuppliedValue}
	options := OptionsForEnvironment(pueue.Options{Environment: supplied}, os.Environ())
	supplied[0] = snapshotMarkerKey + "=mutated-after-snapshot"
	if got := observeSnapshotMarker(t, options); got != snapshotSuppliedValue {
		t.Fatalf("client child observed marker %q after caller mutation, want %q", got, snapshotSuppliedValue)
	}
}

func TestOptionsForEnvironmentWithProcessScopeKeepsExplicitEmptyWithoutAmbientFallback(t *testing.T) {
	t.Setenv(snapshotMarkerKey, snapshotInheritedValue)
	options := OptionsForEnvironment(pueue.Options{Environment: []string{}}, os.Environ())
	if options.Environment == nil || len(options.Environment) != 0 {
		t.Fatalf("explicit empty control environment became %q", options.Environment)
	}
}

// snapshotClientConfig serializes the pueue settings fields the binding reads.
// The pueue parser accepts JSON because it is valid YAML.
type snapshotClientConfig struct {
	Shared snapshotSharedConfig `json:"shared"`
}

type snapshotSharedConfig struct {
	PueueDirectory   string `json:"pueue_directory"`
	RuntimeDirectory string `json:"runtime_directory"`
	UnixSocketPath   string `json:"unix_socket_path"`
	AliasFile        string `json:"alias_file"`
	PIDPath          string `json:"pid_path"`
	SharedSecretPath string `json:"shared_secret_path"`
	DaemonCert       string `json:"daemon_cert"`
	DaemonKey        string `json:"daemon_key"`
}

// snapshotFixtureRoot returns a physical, short, test-owned directory. Binding
// rejects symlinked components (macOS TMPDIR is a /var alias of /private/var)
// and Unix socket paths have a platform length limit, so the default long
// t.TempDir name is not usable for the configured socket path.
func snapshotFixtureRoot(t *testing.T) string {
	t.Helper()
	created, err := os.MkdirTemp("", "dlp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(created); removeErr != nil {
			t.Error(removeErr)
		}
	})
	root, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func observeSnapshotMarker(t *testing.T, options pueue.Options) string {
	t.Helper()
	base := snapshotFixtureRoot(t)
	executable := filepath.Join(base, "fake-pueue")
	if err := os.WriteFile(executable, []byte(snapshotClientScript), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "pueue.json")
	config, err := json.Marshal(snapshotClientConfig{Shared: snapshotSharedConfig{
		PueueDirectory:   filepath.Join(base, "data"),
		RuntimeDirectory: filepath.Join(base, "run"),
		UnixSocketPath:   filepath.Join(base, "socket"),
		AliasFile:        filepath.Join(base, "aliases"),
		PIDPath:          filepath.Join(base, "pid"),
		SharedSecretPath: filepath.Join(base, "secret"),
		DaemonCert:       filepath.Join(base, "cert"),
		DaemonKey:        filepath.Join(base, "key"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := pueue.Bind(context.Background(), executable, configPath, options)
	if err != nil {
		t.Fatalf("bind fake client: %v", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	marker, ok := strings.CutPrefix(client.Binding().ObservedVersion, "pueue marker=")
	if !ok {
		t.Fatal("fake client reported unexpected version output")
	}
	return marker
}
