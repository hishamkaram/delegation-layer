package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type upgradeRealGateFixture struct {
	owner  parentMigrationFixture
	client *pueue.Client
	adds   *atomic.Int32
	pin    taskdir.RootPin
}

func newUpgradeRealGateFixture(t *testing.T, kind string) upgradeRealGateFixture {
	t.Helper()
	f, _ := newWarmContinuationFixture(t)
	// This installed test role acknowledges add but never executes a worker.
	source := strings.Replace(parentMigrationClient, "  *) exit 64 ;;", "  add) printf '%s\\n' 7 ;;\n  *) exit 64 ;;", 1)
	if err := os.WriteFile(f.target.ClientExecutable, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
	adds := &atomic.Int32{}
	f.options.Observer = func(event pueue.CommandEvent) {
		if event.Stage == "started" && slices.Contains(event.Argv, "add") && !slices.Contains(event.Argv, "--help") {
			adds.Add(1)
		}
	}
	var client *pueue.Client
	var err error
	switch kind {
	case "alias":
		writeContinuationAlias(t, f, f.target)
		client, _, err = pueue.RecoverSavedWithPair(t.Context(), f.store.Root, f.target, f.target.ClientExecutable, f.target.DaemonExecutable, f.options)
	case "temporary":
		changeUpgradeRealGateFixtureEndpoint(t, f.target.ConfigPath)
		client, err = pueue.BindSavedForMigrationWithClient(t.Context(), f.target, f.target.ClientExecutable, f.options)
	default:
		t.Fatalf("unknown retained transport %q", kind)
	}
	if err != nil || client == nil {
		t.Fatalf("retained transport setup: client=%t err=%v", client != nil, err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if client.Binding() != f.target || client.RuntimeBinding().ConfigPath == f.target.ConfigPath || pueue.IsPrivateConfig(f.store.Root, client.RuntimeBinding().ConfigPath) {
		t.Fatal("fixture did not retain private logical owner with non-primary runtime config")
	}
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return upgradeRealGateFixture{owner: f, client: client, adds: adds, pin: pin}
}

func changeUpgradeRealGateFixtureEndpoint(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config savedRoutingFixtureConfig
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	config.Shared.UnixSocketPath = filepath.Join(filepath.Dir(path), "next.sock")
	writeContinuationFixtureRecord(t, path, config)
}

// A frozen valid journal is a durable input fixture. These tests exercise the
// public submission boundary, not the upgrade owner's admission policy.
func commitRealGateUpgradeFence(t *testing.T, f upgradeRealGateFixture) {
	t.Helper()
	type rootSubject struct {
		Path   string `json:"path"`
		ID     string `json:"id"`
		Device uint64 `json:"device"`
		Inode  uint64 `json:"inode"`
	}
	type drainSubject struct {
		Root        rootSubject        `json:"root"`
		Predecessor task.SupervisorRef `json:"predecessor"`
		OperationID string             `json:"operation_id"`
		Mode        string             `json:"mode"`
		Phase       string             `json:"phase"`
	}
	record := struct {
		SchemaVersion int          `json:"schema_version"`
		Drain         drainSubject `json:"drain_v1"`
	}{task.SchemaVersion, drainSubject{rootSubject{f.pin.Root(), f.pin.RootID(), f.pin.Device(), f.pin.Inode()}, f.client.Binding(), "upgrade-v1:" + strings.Repeat("d", 32), "operation_required", "draining"}}
	err := pueue.WithExistingBootstrapLockContext(t.Context(), f.pin.Root(), func(context.Context) error {
		writeContinuationFixtureRecord(t, filepath.Join(filepath.Dir(f.owner.target.ConfigPath), "bootstrap.pending"), record)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func prepareRealGateUpgradeSubmission(t *testing.T, f upgradeRealGateFixture) (*taskdir.SubmissionPermit, pueue.Launch) {
	t.Helper()
	subject := routedMetadataSourceForTask(t, f.owner, f.client.Binding(), strings.Repeat("e", 32))
	td, err := f.owner.store.OpenTask(subject.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit, err := td.PrepareSubmission(f.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	return permit, pueue.Launch{RunnerExecutable: f.owner.target.ClientExecutable, RootPath: f.owner.store.Root, Supervised: true}
}

func TestUpgradeSubmissionGateFencesRetainedAliasAndTemporaryTransport(t *testing.T) {
	for _, kind := range []string{"alias", "temporary"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpgradeRealGateFixture(t, kind)
			permit, launch := prepareRealGateUpgradeSubmission(t, f)
			commitRealGateUpgradeFence(t, f)
			_, err := f.client.SubmitWithGate(t.Context(), permit, launch, SubmissionGate(f.owner.store.Root))
			if !errors.Is(err, pueue.ErrPrivateUpgradePending) {
				t.Fatalf("real gate crossed retained %s Drain: adds=%d consumed=%t err=%v", kind, f.adds.Load(), permit.IsConsumed(), err)
			}
			if f.adds.Load() != 0 || permit.IsConsumed() {
				t.Fatalf("Drain reached real-gated add: adds=%d consumed=%t", f.adds.Load(), permit.IsConsumed())
			}
		})
	}
}
