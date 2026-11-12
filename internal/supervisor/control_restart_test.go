package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type restartControlRouting struct {
	controlRoutingFunc
	restart func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.RuntimeRestartReference) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error)
}

func (r restartControlRouting) RestartOwn(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, from infrastructure.RuntimeRestartReference) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error) {
	return r.restart(ctx, store, selected, from)
}

func controlRestartReference(store *taskdir.Store) infrastructure.RuntimeRestartReference {
	// Correlation data only; these tests grant no native startup authority.
	return infrastructure.RuntimeRestartReference{RootID: store.RootID, Startup: infrastructure.StartupReference{
		Sequence: 1, Epoch: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64),
	}}
}

func TestControlRuntimeRestartRequiresDedicatedAuthority(t *testing.T) {
	store := newGenerationStore(t)
	runtime := ControlRuntime{Routing: controlRoutingFunc(nil)}
	if _, _, err := runtime.RestartOwn(t.Context(), store, controlRestartReference(store)); !errors.Is(err, ErrInfrastructureAuthorityUnavailable) {
		t.Fatalf("ordinary routing became restart authority: %v", err)
	}
	from := controlRestartReference(store)
	from.RootID = strings.Repeat("c", 32)
	if _, _, err := runtime.RestartOwn(t.Context(), store, from); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("foreign root reached routing or executable selection: %v", err)
	}
}

func TestControlRuntimeRestartPreservesUnknownContactWithoutRetry(t *testing.T) {
	store := newGenerationStore(t)
	from := controlRestartReference(store)
	unknown := errors.New("restart contact unknown")
	calls := 0
	routing := restartControlRouting{restart: func(ctx context.Context, borrowed *taskdir.Store, selected infrastructure.LaunchContext, observed infrastructure.RuntimeRestartReference) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error) {
		calls++
		if borrowed != store || observed != from || selected.RootID != from.RootID {
			t.Fatal("restart changed target, subject or borrowed store")
		}
		if _, finite := ctx.Deadline(); !finite {
			t.Fatal("restart lost finite control deadline")
		}
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{From: from.Startup, Phase: infrastructure.RuntimeRestartDaemonClosed}, unknown
	}}
	runtime := ControlRuntime{Routing: routing, Lookup: controlRoutingLookup(t)}
	_, report, err := runtime.RestartOwn(t.Context(), store, from)
	if !errors.Is(err, unknown) || calls != 1 || report.Phase != infrastructure.RuntimeRestartDaemonClosed {
		t.Fatalf("uncertain contact replayed or lost observation: calls%d report%+v err%v", calls, report, err)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncertain contact entered local bootstrap: %v", err)
	}
}

func TestControlRuntimeRestartRejectsPrematureReadiness(t *testing.T) {
	store := newGenerationStore(t)
	from := controlRestartReference(store)
	routing := restartControlRouting{restart: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.RuntimeRestartReference) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error) {
		return infrastructure.Disposition{Kind: infrastructure.DispositionStarted}, infrastructure.RuntimeRestartReport{From: from.Startup, Phase: infrastructure.RuntimeRestartDaemonClosed}, nil
	}}
	runtime := ControlRuntime{Routing: routing, Lookup: controlRoutingLookup(t)}
	disposition, report, err := runtime.RestartOwn(t.Context(), store, from)
	if !errors.Is(err, infrastructure.ErrIdentity) || disposition != (infrastructure.Disposition{}) || report != (infrastructure.RuntimeRestartReport{}) {
		t.Fatalf("non-ready reply became readiness: %+v %+v %v", disposition, report, err)
	}
}
