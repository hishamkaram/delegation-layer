package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func appInvocationOrigin(t *testing.T) (infrastructure.Origin, []string) {
	t.Helper()
	origin, err := infrastructure.NewOrigin(filepath.Join(t.TempDir(), "unopened-origin"), strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := infrastructure.WithOrigin([]string{"INVOCATION_SELECTED=configuration"}, origin)
	if err != nil {
		t.Fatal(err)
	}
	return origin, environment
}

func setAppInvocationOrigin(t *testing.T, environment []string) {
	t.Helper()
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && key == infrastructure.OriginEnvironment {
			t.Setenv(key, value)
			return
		}
	}
	t.Fatal("fixture has no origin locator")
}

func unsetAppInvocationOrigin(t *testing.T) {
	t.Helper()
	previous, present := os.LookupEnv(infrastructure.OriginEnvironment)
	t.Cleanup(func() {
		var err error
		if present {
			err = os.Setenv(infrastructure.OriginEnvironment, previous)
		} else {
			err = os.Unsetenv(infrastructure.OriginEnvironment)
		}
		if err != nil {
			t.Error("restore invocation origin environment")
		}
	})
	if err := os.Unsetenv(infrastructure.OriginEnvironment); err != nil {
		t.Fatal("unset invocation origin environment")
	}
}

func TestAppInvocationCaptureSurvivesSelectedAndRecordedEnvironments(t *testing.T) {
	original, actual := appInvocationOrigin(t)
	_, selected := appInvocationOrigin(t)
	setAppInvocationOrigin(t, actual)
	deps := (Dependencies{Environment: selected}).withCapturedEnvironment()
	if deps.invocationSource == nil || deps.invocationSource.err != nil {
		t.Fatal("actual invocation source was not captured")
	}
	if observed, found := deps.invocationSource.source.Origin(); !found || observed != original {
		t.Fatal("selected environment replaced actual invocation origin")
	}
	if !slices.Equal(deps.Environment, selected) {
		t.Fatal("invocation capture replaced selected launch configuration")
	}
	setAppInvocationOrigin(t, selected)
	repeated := deps.withCapturedEnvironment()
	_, recorded := appInvocationOrigin(t)
	scoped, err := scopedDependencies(repeated, recorded, true)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.invocationSource != deps.invocationSource || scoped.invocationSource != deps.invocationSource || scoped.normalized().invocationSource != deps.invocationSource {
		t.Fatal("dependency copies replaced captured invocation owner")
	}
	if scoped.storeDependencies().invocationSource != deps.invocationSource || !slices.Equal(scoped.Environment, recorded) {
		t.Fatal("store projection lost invocation owner or recorded launch scope")
	}
	if observed, found := scoped.invocationSource.source.Origin(); !found || observed != original {
		t.Fatal("environment rebasing replaced original invocation locator")
	}
}

func TestAppInvocationCaptureRetainsOriginalError(t *testing.T) {
	t.Setenv(infrastructure.OriginEnvironment, "malformed original invocation")
	deps := (Dependencies{Environment: []string{}}).withCapturedEnvironment()
	if deps.invocationSource == nil || !errors.Is(deps.invocationSource.err, infrastructure.ErrIdentity) {
		t.Fatal("invocation capture did not retain malformed-origin failure")
	}
	_, valid := appInvocationOrigin(t)
	setAppInvocationOrigin(t, valid)
	repeated := deps.withCapturedEnvironment()
	scoped, err := scopedDependencies(repeated, valid, true)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.invocationSource != deps.invocationSource || scoped.invocationSource != deps.invocationSource || scoped.storeDependencies().invocationSource != deps.invocationSource {
		t.Fatal("later environment recaptured failed invocation")
	}
	if !errors.Is(scoped.invocationSource.err, infrastructure.ErrIdentity) {
		t.Fatal("later valid locator erased original capture failure")
	}
	if _, found := scoped.invocationSource.source.Origin(); found {
		t.Fatal("failed invocation acquired later routing data")
	}
}

func TestAppInvocationCaptureUsesCallerContextOnce(t *testing.T) {
	t.Setenv(infrastructure.OriginEnvironment, "malformed invocation")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	deps := (Dependencies{Environment: []string{}}).withCapturedEnvironmentContext(canceled)
	if deps.invocationSource == nil || !errors.Is(deps.invocationSource.err, context.Canceled) {
		t.Fatal("capture discarded command context before observing invocation")
	}
	repeated := deps.withCapturedEnvironmentContext(t.Context())
	if repeated.invocationSource != deps.invocationSource || !errors.Is(repeated.invocationSource.err, context.Canceled) {
		t.Fatal("later live context recaptured canceled invocation")
	}
	_, valid := appInvocationOrigin(t)
	setAppInvocationOrigin(t, valid)
	accepted := (Dependencies{Environment: []string{}}).withCapturedEnvironmentContext(t.Context())
	if accepted.invocationSource == nil || accepted.invocationSource.err != nil {
		t.Fatal("live command context failed capture")
	}
	later := accepted.withCapturedEnvironmentContext(canceled)
	if later.invocationSource != accepted.invocationSource || later.invocationSource.err != nil {
		t.Fatal("later cancellation replaced original successful capture")
	}
}

func TestAppInvocationInjectedEnvironmentCannotGrantOutsideProvenance(t *testing.T) {
	unsetAppInvocationOrigin(t)
	t.Setenv("DELEGATION_LAYER_PROCESS_SCOPE", "pg:"+strings.Repeat("a", 32))
	_, selected := appInvocationOrigin(t)
	for _, environment := range [][]string{nil, {}, selected} {
		deps := (Dependencies{Environment: environment}).withCapturedEnvironment()
		if deps.invocationSource == nil || !errors.Is(deps.invocationSource.err, lifetime.ErrTaskContained) {
			t.Fatal("injected launch environment erased actual task containment")
		}
		if _, found := deps.invocationSource.source.Origin(); found {
			t.Fatal("injected launch environment invented invocation routing")
		}
		if deps.Environment == nil {
			t.Fatal("launch environment remained uncaptured")
		}
		if environment != nil && !slices.Equal(deps.Environment, environment) {
			t.Fatal("source capture changed explicit launch environment")
		}
	}
}

func TestAppInvocationCaptureErrorStaysLazyForPassiveCommands(t *testing.T) {
	_, valid := appInvocationOrigin(t)
	setAppInvocationOrigin(t, valid)
	validDeps := (Dependencies{Environment: []string{}}).withCapturedEnvironment()
	t.Setenv(infrastructure.OriginEnvironment, "malformed invocation")
	faultDeps := (Dependencies{Environment: []string{}}).withCapturedEnvironment()
	if faultDeps.invocationSource == nil || !errors.Is(faultDeps.invocationSource.err, infrastructure.ErrIdentity) {
		t.Fatal("fixture did not capture invocation error")
	}
	root := filepath.Join(t.TempDir(), "absent-state")
	for _, args := range [][]string{
		{"help"},
		{"version"},
		{"providers", "--json"},
		{"supervisor", "status", "--root", root, "--json"},
	} {
		t.Run(strings.Join(args[:1], ""), func(t *testing.T) {
			var expected, expectedErrors, actual, actualErrors bytes.Buffer
			wantCode := Run(args, &expected, &expectedErrors, validDeps)
			code := Run(args, &actual, &actualErrors, faultDeps)
			if wantCode != 0 || code != wantCode || !bytes.Equal(expected.Bytes(), actual.Bytes()) || !bytes.Equal(expectedErrors.Bytes(), actualErrors.Bytes()) {
				t.Fatalf("passive command changed with irrelevant capture error: want=%d got=%d", wantCode, code)
			}
		})
	}
}
