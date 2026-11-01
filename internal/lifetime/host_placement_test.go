package lifetime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHostPlacementRejectsEveryExplicitTaskMarker(t *testing.T) {
	marker := processScopeMarkerEnvironment + "=pg:" + strings.Repeat("a", 32)
	for _, environment := range [][]string{{marker}, {marker, processScopeMarkerEnvironment + "="}} {
		called := false
		err := checkIndependentHost(t.Context(), environment, func(context.Context) error {
			called = true
			return nil
		})
		if !errors.Is(err, ErrHostContained) || called || strings.Contains(err.Error(), marker) {
			t.Fatalf("positive containment lost or environment exposed: err=%v native=%t", err, called)
		}
	}
}

func TestHostPlacementUsesCapturedEnvironmentAndPreservesInspectionFaults(t *testing.T) {
	t.Setenv(processScopeMarkerEnvironment, "pg:"+strings.Repeat("a", 32))
	fault := errors.New("native observation unavailable")
	err := checkIndependentHost(t.Context(), []string{}, func(context.Context) error { return fault })
	if !errors.Is(err, fault) || errors.Is(err, ErrHostContained) {
		t.Fatalf("ambient environment or containment replaced the inspection: %v", err)
	}
	if err = checkIndependentHost(t.Context(), nil, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("nil captured environment fell back to ambient values: %v", err)
	}
}

func TestHostPlacementRetainsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CheckIndependentHost(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	var absent context.Context // Deliberately exercise the invalid context boundary.
	if err := CheckIndependentHost(absent, nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil context was accepted: %v", err)
	}
}
