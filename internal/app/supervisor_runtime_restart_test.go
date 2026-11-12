package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func runtimeAuthorizationFixture(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, pueue.RuntimeRestartReference, task.SupervisorRef) {
	t.Helper()
	store, td, _ := newAppTestTask(t, false)
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	binding := meta.SupervisorConfig
	binding.ConfigPath = pueue.PrivateConfigPath(store.Root)
	from := seedRuntimeAuthorizationIntent(t, store, 1)
	return store, td, from, binding
}

func seedRuntimeAuthorizationIntent(t *testing.T, store *taskdir.Store, sequence uint64) pueue.RuntimeRestartReference {
	t.Helper()
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Immutable subject data only; no native owner or daemon authority is claimed.
	inputs := infrastructure.StartupInputs{
		Kind: infrastructure.StartupRootOwner, Root: pin.Root(), RootID: pin.RootID(),
		RootDevice: pin.Device(), RootInode: pin.Inode(),
		BootID:    "linux:11111111-2222-3333-4444-555555555555",
		Artifacts: infrastructure.Artifacts{Runner: filepath.Join(store.Root, "fixture-runner")},
		Context:   infrastructure.LaunchContextReference{SHA256: strings.Repeat("a", 64)},
	}
	digest, err := inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	intent := infrastructure.StartupIntent{
		Protocol: infrastructure.RecordProtocolVersion, Sequence: sequence, Epoch: fmt.Sprintf("%032x", sequence),
		Inputs: inputs, InputsSHA256: digest,
		AcceptedAt: "2026-11-03T00:00:00Z", Deadline: "2026-11-03T00:00:30Z",
	}
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := control.PutContext(t.Context(), fmt.Sprintf("intent-%020d.json", intent.Sequence), intent)
	if err = errors.Join(writeErr, control.Close()); err != nil {
		t.Fatal(err)
	}
	startup, err := infrastructure.ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	token, err := infrastructure.FormatRuntimeRestartReference(infrastructure.RuntimeRestartReference{RootID: store.RootID, Startup: startup})
	if err != nil {
		t.Fatal(err)
	}
	return pueue.RuntimeRestartReference(token)
}

func TestRuntimeRestartAuthorizationRequiresExactFenceForPreparation(t *testing.T) {
	store, td, from, binding := runtimeAuthorizationFixture(t)
	err := pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		if err := authorizePrivateRuntimeRestart(ctx, store, from, binding, nil); !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
			t.Fatalf("unfenced prepared task bypassed strict closure: %v", err)
		}
		writeRuntimeRestartFenceFixture(t, ctx, store.Root, binding, from)
		return authorizePrivateRuntimeRestart(ctx, store, from, binding, nil)
	})
	if err != nil {
		t.Fatalf("exact fenced preparation could not be observed: %v", err)
	}
	observed, err := store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
	if err != nil || observed.Proof != nil {
		t.Fatalf("fenced preparation fabricated closure: %+v %v", observed, err)
	}
}

func TestRuntimeRestartAuthorizationRefusesUnownedBootstrap(t *testing.T) {
	store, _, from, binding := runtimeAuthorizationFixture(t)
	writeRuntimeRestartFenceFixture(t, t.Context(), store.Root, binding, from)
	if err := authorizePrivateRuntimeRestart(t.Context(), store, from, binding, nil); err == nil {
		t.Fatal("matching journal replaced actual bootstrap authority")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := authorizePrivateRuntimeRestart(ctx, store, from, binding, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled restart policy returned %v", err)
	}
}

func TestRuntimeRestartAuthorizationRefusesMismatchedOrBusyFence(t *testing.T) {
	for _, subject := range []string{"reference", "binding", "queue"} {
		t.Run(subject, func(t *testing.T) {
			store, _, from, binding := runtimeAuthorizationFixture(t)
			writeRuntimeRestartFenceFixture(t, t.Context(), store.Root, binding, from)
			var jobs []pueue.Job
			switch subject {
			case "reference":
				from = seedRuntimeAuthorizationIntent(t, store, 2)
			case "binding":
				binding.Endpoint = "unix:" + filepath.Join(store.Root, "other.sock")
			case "queue":
				jobs = []pueue.Job{{ID: 1, State: pueue.StateRunning}}
			}
			err := pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
				return authorizePrivateRuntimeRestart(ctx, store, from, binding, jobs)
			})
			if err == nil {
				t.Fatal("mismatched subject or active queue was authorized")
			}
			if subject == "queue" && !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
				t.Fatalf("active queue refusal lost busy classification: %v", err)
			}
			if _, err = store.PinRootContext(t.Context()); err != nil {
				t.Fatalf("policy closed its borrowed store: %v", err)
			}
		})
	}
}
