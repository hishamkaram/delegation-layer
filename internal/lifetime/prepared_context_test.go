//go:build linux || darwin

package lifetime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreatePreparedContextRejectsMissingOrCanceledAuthority(t *testing.T) {
	plan := processGroupPlan("context-root", "task", "context-task")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()
	for _, test := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"nil", nil, ErrNilContext},
		{"canceled", canceled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope, err := CreatePreparedContext(test.ctx, plan)
			if scope != nil {
				releasePreparedContextScope(t, scope)
			}
			if scope != nil || !errors.Is(err, test.want) {
				t.Fatalf("creation without authority: scope=%v err=%v", scope, err)
			}
		})
	}
}

func TestCreatePreparedPlatformPropagatesContextToNativeOpen(t *testing.T) {
	plan := processGroupPlan("platform-context-root", "task", "platform-context-task")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Bypass the public entry gate: the platform must pass this context to its
	// native process-group opener rather than silently replacing it.
	scope, err := createPreparedPlatformScope(ctx, plan)
	if scope != nil {
		releasePreparedContextScope(t, scope)
	}
	if scope != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("platform discarded canceled native-open context: scope=%v err=%v", scope, err)
	}
}

func TestCreatePreparedContextRetainsNativePlanAndLegacyWrapper(t *testing.T) {
	plan := processGroupPlan("native-context-root", "task", "native-context-task")
	for _, create := range []func(Plan) (*Scope, error){
		func(plan Plan) (*Scope, error) { return CreatePreparedContext(t.Context(), plan) },
		CreatePrepared,
	} {
		scope, err := create(plan)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { releasePreparedContextScope(t, scope) })
		ref := scope.Reference()
		if ref.Kind() != plan.Kind() || ref.Assurance() != plan.Assurance() || ref.Identifier() != plan.Identifier() {
			t.Fatalf("context-aware creation changed the frozen native plan: %+v", ref)
		}
	}
}

func TestCreatePreparedContextRejectsInvalidPlan(t *testing.T) {
	if scope, err := CreatePreparedContext(t.Context(), Plan{}); scope != nil || !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("invalid native plan accepted: %v %v", scope, err)
	}
}

func releasePreparedContextScope(t *testing.T, scope *Scope) {
	t.Helper()
	if err := errors.Join(scope.DestroyEmpty(), scope.Release()); err != nil {
		t.Error(err)
	}
}

func TestOpenPreparedContextRejectsMissingOrCanceledAuthority(t *testing.T) {
	plan := processGroupPlan("open-context-root", "task", "open-context-task")
	var missing context.Context
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()
	for _, test := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"nil", missing, ErrNilContext},
		{"canceled", canceled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope, err := OpenPreparedContext(test.ctx, plan)
			if scope != nil {
				if releaseErr := scope.Release(); releaseErr != nil {
					t.Error(releaseErr)
				}
			}
			if scope != nil || !errors.Is(err, test.want) {
				t.Fatalf("recovery without authority: scope=%v err=%v, want %v", scope, err, test.want)
			}
		})
	}
}

func TestOpenPreparedContextProcessGroupRemainsAbsentWithLegacyWrapper(t *testing.T) {
	plan := processGroupPlan("open-active-root", "task", "open-active-task")
	for _, test := range []struct {
		name string
		open func(Plan) (*Scope, error)
	}{
		{"active context", func(plan Plan) (*Scope, error) { return OpenPreparedContext(t.Context(), plan) }},
		{"legacy wrapper", OpenPrepared},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope, err := test.open(plan)
			if scope != nil {
				if releaseErr := scope.Release(); releaseErr != nil {
					t.Error(releaseErr)
				}
			}
			if scope != nil || !errors.Is(err, ErrScopeAbsent) {
				t.Fatalf("process-group recovery invented a persistent resource: scope=%v err=%v", scope, err)
			}
		})
	}
}

func TestOpenPreparedContextRejectsInvalidPlan(t *testing.T) {
	if scope, err := OpenPreparedContext(t.Context(), Plan{}); scope != nil || !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("invalid recovery plan accepted: scope=%v err=%v", scope, err)
	}
}
