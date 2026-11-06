package infrastructure

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type preparationTestGuard func(context.Context, string, func() error) error

func (g preparationTestGuard) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return g(ctx, root, start)
}

type preparationObservationCase struct {
	name    string
	start   error
	guard   error
	started bool
}

func TestPreparationObservationPreservesStartAndGuardResults(t *testing.T) {
	startFault := errors.New("native Start refused")
	guardFault := errors.New("guard failed after callback")
	for _, fixture := range []preparationObservationCase{
		{"successful Start", nil, nil, true},
		{"failed Start", startFault, nil, false},
		{"post-Start guard error", nil, guardFault, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			checkPreparationObservationCase(t, fixture)
		})
	}
}

func TestPreparationObservationWarmReuseRequiresCanonicalReady(t *testing.T) {
	guard := preparationTestGuard(func(context.Context, string, func() error) error {
		t.Fatal("warm reuse invoked native guard")
		return nil
	})
	observation, err := NewPreparationObservation(guard, LaunchContextReference{SHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Disposition() != (Disposition{}) {
		t.Fatal("absence of Start was treated as canonical readiness")
	}
	observation.RecordReady()
	if got := observation.Disposition(); got != (Disposition{Kind: DispositionWarmReuse}) {
		t.Fatal("canonical warm reuse acquired an unapplied context")
	}
}

func checkPreparationObservationCase(t *testing.T, fixture preparationObservationCase) {
	t.Helper()
	selected := LaunchContextReference{SHA256: strings.Repeat("a", 64)}
	guard := preparationTestGuard(func(ctx context.Context, root string, start func() error) error {
		if root != "/operation-root" || ctx != t.Context() {
			t.Fatal("observation replaced native guard inputs")
		}
		if err := start(); err != nil {
			return err
		}
		return fixture.guard
	})
	observation, err := NewPreparationObservation(guard, selected)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	wantError := fixture.start
	if wantError == nil {
		wantError = fixture.guard
	}
	err = observation.GuardDaemonStart(t.Context(), "/operation-root", func() error {
		calls++
		return fixture.start
	})
	if calls != 1 || !errors.Is(err, wantError) {
		t.Fatalf("Start/guard result changed: calls %d, error %v", calls, err)
	}
	got := observation.Disposition()
	if fixture.started {
		if got != (Disposition{Kind: DispositionStarted, Context: selected}) {
			t.Fatal("successful Start was lost after the guard returned")
		}
		observation.RecordReady()
		if observation.Disposition() != got {
			t.Fatal("readiness erased successful Start provenance")
		}
	} else if got != (Disposition{}) {
		t.Fatal("failed Start was reported as a verified outcome")
	}
}
