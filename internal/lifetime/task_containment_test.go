package lifetime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCurrentTaskContainmentMarkerPolicy(t *testing.T) {
	marker := processScopeMarkerEnvironment + "=pg:" + strings.Repeat("a", 32)
	for _, tc := range []struct {
		name        string
		environment []string
		contained   bool
	}{
		{name: "valid", environment: []string{marker}, contained: true},
		{name: "duplicate last empty", environment: []string{marker, processScopeMarkerEnvironment + "="}, contained: true},
		{name: "duplicate first malformed", environment: []string{processScopeMarkerEnvironment + "=bad", marker}, contained: true},
		{name: "malformed", environment: []string{processScopeMarkerEnvironment + "=pg:short"}},
		{name: "near prefix", environment: []string{"X" + marker}},
		{name: "no value", environment: []string{processScopeMarkerEnvironment}},
		{name: "empty", environment: []string{}},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := checkCurrentTaskContainment(t.Context(), tc.environment, func(context.Context) error { calls++; return nil })
			if errors.Is(err, ErrTaskContained) != tc.contained || (err != nil) != tc.contained {
				t.Fatalf("containment = %v, want %t", err, tc.contained)
			}
			if tc.contained && calls != 0 || !tc.contained && calls != 1 {
				t.Fatalf("native observations = %d", calls)
			}
			if err != nil && (errors.Is(err, ErrHostContained) || strings.Contains(err.Error(), marker)) {
				t.Fatalf("neutral containment leaked scheduler policy or marker: %v", err)
			}
		})
	}
}

func TestCurrentTaskContainmentPreservesContextAndNativeFaults(t *testing.T) {
	t.Setenv(processScopeMarkerEnvironment, "pg:"+strings.Repeat("a", 32))
	fault := errors.New("native observation unavailable")
	for _, environment := range [][]string{nil, {}} {
		err := checkCurrentTaskContainment(t.Context(), environment, func(context.Context) error { return fault })
		if !errors.Is(err, fault) || errors.Is(err, ErrTaskContained) || errors.Is(err, ErrHostContained) {
			t.Fatalf("native fault replaced = %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CheckCurrentTaskContainment(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	var absent context.Context // Deliberately exercise the invalid context boundary.
	if err := CheckCurrentTaskContainment(absent, nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil context accepted: %v", err)
	}
}

func TestIndependentHostAdaptsNeutralContainment(t *testing.T) {
	calls := 0
	err := checkIndependentHost(t.Context(), []string{}, func(context.Context) error { calls++; return ErrTaskContained })
	want := "scheduler host is inside a Delegate task scope: run scheduler serve yourself from an independent terminal or tmux session"
	if !errors.Is(err, ErrHostContained) || err.Error() != want || calls != 1 {
		t.Fatalf("scheduler adapter = %v, calls=%d", err, calls)
	}
	marker := processScopeMarkerEnvironment + "=pg:" + strings.Repeat("a", 32)
	if err := CheckIndependentHost(t.Context(), []string{marker}); !errors.Is(err, ErrHostContained) || err.Error() != want {
		t.Fatalf("public scheduler guidance changed: %v", err)
	}
	if err := CheckCurrentTaskContainment(t.Context(), []string{marker}); !errors.Is(err, ErrTaskContained) || errors.Is(err, ErrHostContained) {
		t.Fatalf("public neutral classification = %v", err)
	}
}
