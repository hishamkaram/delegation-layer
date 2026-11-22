package app

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestGatewayMaintenanceGuardsExistingRootBeforeMutation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	token, err := store.RootTokenContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	maintenance := gatewayNativeMaintenance{dependencies: Dependencies{}.withCapturedEnvironmentContext(ctx).normalized()}
	for _, test := range []struct{ name, root, token string }{
		{"wrong root token", store.Root, token + "wrong"},
		{"missing root", filepath.Join(t.TempDir(), "absent"), token},
		{"missing guard", store.Root, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := supervisorfixture.SnapshotStore(t, store.Root)
			observed, err := maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: test.root, ExpectedRoot: test.token})
			if err == nil || observed != (gatewaycommand.NativeTransition{}) {
				t.Fatalf("guarded maintenance accepted root: observation=%+v err=%v", observed, err)
			}
			if test.root == store.Root && !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("saved-root mismatch lost its identity cause: %v", err)
			}
			if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
				t.Fatal("guard refusal changed the existing Native root")
			}
			if test.root != store.Root {
				if _, statErr := os.Lstat(test.root); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("guarded request bootstrapped missing root: %v", statErr)
				}
			}
		})
	}
}

func TestGatewayMaintenanceRejectsReplacedRootAtSamePath(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root := filepath.Join(t.TempDir(), "state")
	original, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	token, tokenErr := original.RootTokenContext(ctx)
	if err = errors.Join(tokenErr, original.Close()); err != nil {
		t.Fatal(err)
	}
	maintenance := gatewayNativeMaintenance{dependencies: Dependencies{}.withCapturedEnvironmentContext(ctx).normalized()}
	if err = os.Rename(root, root+"-retained"); err != nil {
		t.Fatal(err)
	}
	replacement, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := replacement.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before := supervisorfixture.SnapshotStore(t, root)
	_, err = maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: root, ExpectedRoot: token})
	if !errors.Is(err, task.ErrIdentityMismatch) || !maps.Equal(before, supervisorfixture.SnapshotStore(t, root)) {
		t.Fatalf("saved token admitted a replacement root or mutated it: %v", err)
	}
}

func TestGatewayMaintenancePreservesCapturedCallerBeforeReplay(t *testing.T) {
	for _, name := range []string{"contained", "origin"} {
		t.Run(name, func(t *testing.T) {
			store := supervisorfixture.NewInspectionStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			token, err := store.RootTokenContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := configureGatewayMaintenanceCaller(t, store.Root, store.RootID, name)
			maintenance := gatewayNativeMaintenance{dependencies: Dependencies{}.withCapturedEnvironmentContext(ctx).normalized()}
			before := supervisorfixture.SnapshotStore(t, store.Root)
			// A caller-known exact operation requests the replay path. Invocation
			// rejection must win before any history lookup or no-op decision.
			_, err = maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: store.Root, ExpectedRoot: token, OperationID: "upgrade-v1:" + strings.Repeat("a", 32)})
			if !errors.Is(err, want) || !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
				t.Fatalf("captured caller escaped refusal: err=%v want=%v", err, want)
			}
		})
	}
}

func configureGatewayMaintenanceCaller(t *testing.T, root, rootID, kind string) error {
	t.Helper()
	if kind == "contained" {
		t.Setenv("DELEGATION_LAYER_PROCESS_SCOPE", "pg:"+strings.Repeat("c", 32))
		return lifetime.ErrTaskContained
	}
	origin, err := infrastructure.NewOrigin(root, rootID)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := infrastructure.WithOrigin(nil, origin)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	return infrastructure.ErrOwnerRequest
}

func TestGatewayMaintenanceRetainsParentCancellation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	maintenance := gatewayNativeMaintenance{dependencies: Dependencies{}.withCapturedEnvironmentContext(ctx).normalized()}
	token, err := store.RootTokenContext(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	before := supervisorfixture.SnapshotStore(t, store.Root)
	_, err = maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: store.Root, ExpectedRoot: token})
	if !errors.Is(err, context.Canceled) || !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
		t.Fatalf("parent cancellation reset or performed maintenance: %v", err)
	}
}

func TestGatewayMaintenanceObservationDoesNotInventOperation(t *testing.T) {
	for _, control := range []*SupervisorControlResponse{nil, {}, {Upgrade: &SupervisorUpgradeResponse{}}} {
		if got := gatewayMaintenanceObservation(Response{SupervisorControl: control}); got != (gatewaycommand.NativeTransition{}) {
			t.Fatalf("Native busy without operation invented a receipt: %+v", got)
		}
	}
	operation := "upgrade-v1:" + strings.Repeat("a", 32)
	for _, completed := range []bool{false, true} {
		control := &SupervisorControlResponse{Upgrade: &SupervisorUpgradeResponse{OperationID: operation, Phase: "migrating", Completed: completed}}
		got := gatewayMaintenanceObservation(Response{SupervisorControl: control})
		if got.OperationID != operation || got.Phase != "migrating" || got.Pending == completed || got.HandoffNeeded {
			t.Fatalf("Native retained report projection changed: %+v", got)
		}
	}
}
