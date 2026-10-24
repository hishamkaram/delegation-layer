package app

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const runnerScopeMarker = "DELEGATE_APP_SCOPE_MARKER"

// runnerScopeProbe reports only the synthetic marker and PWD. Its absolute
// interpreter needs no PATH, so an empty selected scope still launches.
const runnerScopeProbe = "#!/bin/sh\nprintf 'marker=%s\\npwd=%s\\n' \"${" + runnerScopeMarker + "-unset}\" \"${PWD-unset}\"\n"

func TestRunRunnerNilPlanEnvironmentUsesSelectedLaunchScope(t *testing.T) {
	t.Setenv(runnerScopeMarker, "ambient")
	caller := []string{runnerScopeMarker + "=caller", "PATH=/usr/bin:/bin"}
	tests := []struct {
		name    string
		options runnerRefreshOptions
		plan    []string
		want    string
		// legacyPWD marks the unrecorded launch whose inherited caller scope
		// must still place PWD at the admitted working directory.
		legacyPWD bool
	}{
		{name: "recorded nil", options: runnerRefreshOptions{EnvironmentRecorded: true}, want: "unset"},
		{name: "recorded empty", options: runnerRefreshOptions{Environment: []string{}, EnvironmentRecorded: true}, want: "unset"},
		{name: "legacy unrecorded", options: runnerRefreshOptions{}, want: "caller", legacyPWD: true},
		{name: "explicit empty plan", options: runnerRefreshOptions{}, plan: []string{}, want: "unset"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.options.ProviderScript = runnerScopeProbe
			payload, cwd := runRunnerScopeProbe(t, test.options, test.plan, caller)
			if !strings.Contains(payload, "marker="+test.want+"\n") {
				t.Fatalf("provider marker = %q, want %q", payload, test.want)
			}
			if test.legacyPWD && !strings.Contains(payload, "pwd="+cwd+"\n") {
				t.Fatalf("legacy launch lost PWD %q: %q", cwd, payload)
			}
			if os.Getenv(runnerScopeMarker) != "ambient" {
				t.Fatal("runner changed the process environment")
			}
		})
	}
}

// runRunnerScopeProbe runs one admitted probe task through the runner with an
// explicit caller scope and returns the committed provider payload and the
// admitted working directory.
func runRunnerScopeProbe(t *testing.T, options runnerRefreshOptions, plan, caller []string) (string, string) {
	t.Helper()
	fixture := newRunnerRefreshFixtureWith(t, options)
	defer fixture.close(t)
	recordRunnerSupervisorReceipt(t, fixture, 7)
	deps := Dependencies{
		Environment: caller,
		PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
			profile := fixture.profile()
			profile.Plan.Environment = plan
			return profile, nil
		},
		SupervisorOptions: pueue.Options{Environment: fixture.environment},
	}
	result := runRunner(runnerArguments{Root: fixture.store.Root, TaskID: fixture.req.TaskID}, deps)
	if result.err != nil || result.code != 0 || result.response.Outcome == nil || result.response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("probe runner did not commit: %+v", result)
	}
	inspection, err := fixture.td.Inspect()
	if err != nil || !inspection.StartedExists || !inspection.SealExists {
		t.Fatalf("probe launch evidence = %+v, %v", inspection, err)
	}
	return readRunnerPayload(t, fixture, result.response.Outcome), fixture.req.CanonicalCwd
}

func readRunnerPayload(t *testing.T, fixture *runnerRefreshFixture, outcome *task.OutcomeRecord) string {
	t.Helper()
	reader, err := fixture.td.OpenPayload(outcome)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunRunnerEnvShebangResolvesInterpreterFromSelectedScope(t *testing.T) {
	script := "#!/usr/bin/env sh\nprintf 'marker=%s\\n' \"${" + runnerScopeMarker + "-unset}\"\n"
	caller := []string{runnerScopeMarker + "=caller", "PATH=/usr/bin:/bin"}
	payload, _ := runRunnerScopeProbe(t, runnerRefreshOptions{ProviderScript: script}, nil, caller)
	if !strings.Contains(payload, "marker=caller\n") {
		t.Fatalf("env-shebang provider marker = %q, want caller scope", payload)
	}

	fixture := newRunnerRefreshFixtureWith(t, runnerRefreshOptions{ProviderScript: script, Environment: []string{}, EnvironmentRecorded: true})
	defer fixture.close(t)
	recordRunnerSupervisorReceipt(t, fixture, 7)
	deps := Dependencies{
		Environment:       caller,
		PrepareProfile:    func(task.TaskRecord) (PreparedProfile, error) { return fixture.profile(), nil },
		SupervisorOptions: pueue.Options{Environment: fixture.environment},
	}
	result := runRunner(runnerArguments{Root: fixture.store.Root, TaskID: fixture.req.TaskID}, deps)
	if result.err != nil || result.code != 0 || result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
		t.Fatalf("empty selected scope did not seal a start refusal: %+v", result)
	}
	inspection, err := fixture.td.Inspect()
	if err != nil || inspection.StartedExists || !inspection.SealExists {
		t.Fatalf("empty selected scope launched through the caller PATH: %+v, %v", inspection, err)
	}
	if refusal := readRunnerPayload(t, fixture, result.response.Outcome); !strings.Contains(refusal, `interpreter "sh" is unavailable`) {
		t.Fatalf("empty selected scope refusal = %q", refusal)
	}
}
