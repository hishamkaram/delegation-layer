package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type controlRoutingFunc func(context.Context, *taskdir.Store) (infrastructure.Disposition, error)

func (f controlRoutingFunc) EnsurePrivate(ctx context.Context, store *taskdir.Store, _ infrastructure.LaunchContext) (infrastructure.Disposition, error) {
	return f(ctx, store)
}

func (controlRoutingFunc) RecoverSaved(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	return infrastructure.Disposition{}, errors.New("unexpected saved routing")
}

func (controlRoutingFunc) PrepareContinuation(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	return infrastructure.Disposition{}, errors.New("unexpected continuation routing")
}

func (controlRoutingFunc) MigrateOwn(context.Context, *taskdir.Store, infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, errors.New("unexpected manual migration")
}

func TestControlRuntimeColdAdmissionRequiresIndependentAuthority(t *testing.T) {
	store := newGenerationStore(t)
	runtime := ControlRuntime{}
	if client, err := runtime.PreparePrivateAdmission(t.Context(), store); client != nil || !errors.Is(err, ErrInfrastructureAuthorityUnavailable) {
		t.Fatalf("missing authority entered local preparation: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cold probe created private supervisor state: %v", err)
	}
}

func TestControlRuntimePreparationReplyNeverReplacesReadyProof(t *testing.T) {
	store := newGenerationStore(t)
	calls := 0
	runtime := ControlRuntime{Lookup: controlRoutingLookup(t), Routing: controlRoutingFunc(func(ctx context.Context, borrowed *taskdir.Store) (infrastructure.Disposition, error) {
		calls++
		if borrowed != store {
			return infrastructure.Disposition{}, errors.New("borrowed store replaced")
		}
		if _, finite := ctx.Deadline(); !finite {
			return infrastructure.Disposition{}, errors.New("routing has no finite deadline")
		}
		return infrastructure.Disposition{Kind: infrastructure.DispositionWarmReuse}, nil
	})}
	if client, err := runtime.PreparePrivateAdmission(t.Context(), store); client != nil || !errors.Is(err, ErrInfrastructureAuthorityUnavailable) || calls != 1 {
		t.Fatalf("reply became readiness or triggered retry: calls=%d err=%v", calls, err)
	}
}

func TestControlRuntimeUnknownContactIsPreservedWithoutRetry(t *testing.T) {
	store := newGenerationStore(t)
	unknown := errors.New("unknown preparation outcome")
	calls := 0
	runtime := ControlRuntime{Lookup: controlRoutingLookup(t), Routing: controlRoutingFunc(func(context.Context, *taskdir.Store) (infrastructure.Disposition, error) {
		calls++
		return infrastructure.Disposition{}, unknown
	})}
	if client, err := runtime.PreparePrivateAdmission(t.Context(), store); client != nil || !errors.Is(err, unknown) || calls != 1 {
		t.Fatalf("unknown outcome lost or replayed: calls=%d err=%v", calls, err)
	}
	if _, err := os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown outcome started local preparation: %v", err)
	}
}

func TestControlRuntimeRefusesReplacedBorrowedRootAfterContact(t *testing.T) {
	store := newGenerationStore(t)
	original := store.Root + "-original"
	defer func() {
		if _, err := os.Stat(original); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				t.Error(err)
			}
			return
		}
		if err := os.RemoveAll(store.Root); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(original, store.Root); err != nil {
			t.Error(err)
		}
	}()
	runtime := ControlRuntime{Lookup: controlRoutingLookup(t), Routing: controlRoutingFunc(func(context.Context, *taskdir.Store) (infrastructure.Disposition, error) {
		if err := os.Rename(store.Root, original); err != nil {
			return infrastructure.Disposition{}, err
		}
		if err := os.Mkdir(store.Root, 0o700); err != nil {
			return infrastructure.Disposition{}, err
		}
		return infrastructure.Disposition{Kind: infrastructure.DispositionWarmReuse}, nil
	})}
	if client, err := runtime.PreparePrivateAdmission(t.Context(), store); client != nil || err == nil {
		t.Fatal("replaced borrowed root accepted")
	}
}

// Routing requires an actual selected installed role, even when the resident
// reply cannot prove readiness. Authored finite scripts own no daemon work.
func controlRoutingLookup(t *testing.T) installation.ExecutableLookup {
	t.Helper()
	client, _ := installedPairFixture(t, parentMigrationClient, "#!/bin/sh\nexit 64\n")
	return installation.ExecutableLookup{InitialExecutable: client}
}
