package testsupport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// WriteUpgradeGeneration records canonical DATA for the admission boundary.
// No daemon/process or native capability is claimed. The runtime receipt names
// a different installed client/daemon controlling the same published config.
func WriteUpgradeGeneration(t *testing.T, store *taskdir.Store) (logical, runtime task.SupervisorRef) {
	t.Helper()
	logical = task.SupervisorRef{ClientExecutable: "/installed/old/pueue", DaemonExecutable: "/installed/old/pueued", ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, ConfigPath: pueue.PrivateConfigPath(store.Root), ConfigDigest: strings.Repeat("a", 64), ResolvedConfigSHA256: strings.Repeat("b", 64), Endpoint: "unix:" + filepath.Join(store.Root, "pueue.sock"), ObservedVersion: "pueue 4.0.4"}
	runtime = logical
	runtime.ClientExecutable = "/installed/current/pueue"
	runtime.DaemonExecutable = "/installed/current/pueued"
	base := filepath.Dir(logical.ConfigPath)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "bootstrap.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeUpgradeFixture(t, filepath.Join(base, "supervisor.identity.json"), map[string]any{"schema_version": task.SchemaVersion, "binding": logical})
	writeUpgradeFixture(t, filepath.Join(base, "daemon.identity.json"), map[string]any{"executable": logical.DaemonExecutable, "executable_identity": logical.ExecutableIdentity, "config_path": logical.ConfigPath, "config_digest": logical.ConfigDigest, "resolved_config_sha256": logical.ResolvedConfigSHA256, "endpoint": logical.Endpoint})
	writeUpgradeFixture(t, filepath.Join(base, "ordinary-scheduling.json"), map[string]any{"schema_version": task.SchemaVersion, "revision": "unlimited-v4", "supervisor": runtime, "staged_launch": map[string]bool{"add_stashed": true, "enqueue_by_id": true}})
	return logical, runtime
}

// WriteUpgradeDrain freezes a valid existing-root maintenance request.
func WriteUpgradeDrain(t *testing.T, store *taskdir.Store, predecessor task.SupervisorRef) {
	t.Helper()
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	writeUpgradeFixture(t, filepath.Join(store.Root, ".supervisor", "bootstrap.pending"), map[string]any{"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": predecessor, "operation_id": "upgrade-v1:" + strings.Repeat("9", 32), "mode": "operation_required", "phase": "draining"}})
}

func writeUpgradeFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := task.MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
