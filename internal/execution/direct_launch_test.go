package execution

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// installedProvider is a task whose provider is a private script, so a test
// may rewrite, replace or remove the installed file without touching the test
// binary. refreshed is the path-only plan a fresh runtime preflight selects.
type installedProvider struct {
	binderTaskFixture
	temporary string
	admitted  string
	refreshed Plan
}

// providerChange is one way an installer changes the admitted provider path.
type providerChange struct {
	name  string
	apply func(t *testing.T, path string)
}

var queuedProviderChanges = []providerChange{
	{"rewritten", func(t *testing.T, path string) { writeProviderScript(t, path, "rewritten") }},
	{"renamed-over", func(t *testing.T, path string) {
		next := path + ".next"
		writeProviderScript(t, next, "renamed")
		require(t, os.Rename(next, path))
	}},
	{"removed", func(t *testing.T, path string) { require(t, os.Remove(path)) }},
}

func writeProviderScript(t *testing.T, path, output string) {
	t.Helper()
	require(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0o700))
}

func installedProviderTask(t *testing.T) installedProvider {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	admitted := filepath.Join(root, "provider")
	writeProviderScript(t, admitted, "admitted")
	require(t, os.Mkdir(filepath.Join(root, "next"), 0o700))
	replacement := filepath.Join(root, "next", "provider")
	writeProviderScript(t, replacement, "replacement")
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", func(meta *task.MetaRecord, plan *Plan) {
		meta.ProviderExecutable = admitted
		plan.Executable, plan.Arguments = admitted, nil
	})
	plan.Environment = append(slices.Clone(plan.Environment), "TMPDIR="+temporary)
	refreshed := plan
	refreshed.Executable = replacement
	refreshed.ProviderRuntime = task.ProviderRuntimeIdentity{Executable: replacement}
	return installedProvider{binderTaskFixture: binderTaskFixture{td: td, permit: permit, plan: plan}, temporary: temporary, admitted: admitted, refreshed: refreshed}
}

type directRun struct {
	result     Result
	preflights int
	started    []string
}

// run launches the task once. refresh installs a runtime preflight that
// returns the replacement plan; started records each Start's command path.
func (p installedProvider) run(refresh bool) directRun {
	run := directRun{}
	options := Options{Hooks: Hooks{Start: func(cmd *exec.Cmd) error {
		run.started = append(run.started, cmd.Path)
		return cmd.Start()
	}}}
	if refresh {
		options.PreflightPlan = func(PreflightScope, Plan) (Plan, error) {
			run.preflights++
			return p.refreshed, nil
		}
	}
	run.result = Run(p.td, p.permit, p.plan, options)
	return run
}

// TestQueuedProviderUpdateLaunchesRefreshedInstalledExecutable replaces the
// retired initial-binding deferral tests: nothing is bound before preflight,
// and the one Start runs the installed replacement directly, with no copy,
// and no provider digest.
func TestQueuedProviderUpdateLaunchesRefreshedInstalledExecutable(t *testing.T) {
	for _, change := range queuedProviderChanges {
		t.Run(change.name, func(t *testing.T) {
			provider := installedProviderTask(t)
			change.apply(t, provider.admitted)
			run := provider.run(true)
			assertSuccess(t, run.result)
			if run.preflights != 1 || !slices.Equal(run.started, []string{provider.refreshed.Executable}) {
				t.Fatalf("preflight=%d starts=%q, want one preflight and one direct Start of %s", run.preflights, run.started, provider.refreshed.Executable)
			}
			if got := string(payload(t, provider.td, run.result.Outcome)); got != "replacement\n" {
				t.Fatalf("provider output %q, want the replacement", got)
			}
			assertObservedRuntimeEvidence(t, provider.td, provider.refreshed.ProviderRuntime)
			assertNoTaskViews(t, provider.temporary)
		})
	}
}

// TestCompatibleUpdateWithoutRefreshRunsInstalledBytes replaces the retired
// disabled-refresh refusal: at the same path an update is the user's
// installation and runs; a removed executable is an explicit Start failure
// sealed once, with no retry and no fallback.
func TestCompatibleUpdateWithoutRefreshRunsInstalledBytes(t *testing.T) {
	want := map[string]string{"rewritten": "rewritten\n", "renamed-over": "renamed\n"}
	for _, change := range queuedProviderChanges {
		t.Run(change.name, func(t *testing.T) {
			provider := installedProviderTask(t)
			change.apply(t, provider.admitted)
			run := provider.run(false)
			if !slices.Equal(run.started, []string{provider.admitted}) {
				t.Fatalf("starts=%q, want one direct Start of the admitted path", run.started)
			}
			if output, ok := want[change.name]; ok {
				assertSuccess(t, run.result)
				if got := string(payload(t, provider.td, run.result.Outcome)); got != output {
					t.Fatalf("provider output %q, want %q", got, output)
				}
				return
			}
			assertPendingRuntimeRejected(t, provider.td, run.result)
			if seal := readSeal(t, provider); !strings.Contains(seal.Error, "no such file or directory") {
				t.Fatalf("missing executable sealed %q, want the concrete exec error", seal.Error)
			}
			assertNoTaskViews(t, provider.temporary)
		})
	}
}

func readSeal(t *testing.T, provider installedProvider) task.ProviderExitRecord {
	t.Helper()
	exit, err := os.ReadFile(filepath.Join(provider.td.Dir, "provider.exit"))
	require(t, err)
	var seal task.ProviderExitRecord
	require(t, task.DecodeStrict(exit, &seal))
	return seal
}

// A preflight refusal seals its cause without consuming a native Start.
func TestRunConstructsDirectProviderCommandWithoutCopy(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "installed", true: "preflight-refusal"}[refuse], func(t *testing.T) {
			fixture, temporary := binderTask(t)
			injected := errors.New("selected helper capability unavailable")
			var started *exec.Cmd
			result := Run(fixture.td, fixture.permit, fixture.plan, Options{
				PreflightPlan: func(_ PreflightScope, plan Plan) (Plan, error) {
					if refuse {
						return Plan{}, injected
					}
					return plan, nil
				},
				Hooks: Hooks{Start: func(cmd *exec.Cmd) error { started = cmd; return cmd.Start() }},
			})
			if refuse {
				assertHelperRefusalSealed(t, fixture, result, started, injected)
			} else {
				assertSuccess(t, result)
				assertDirectProviderCommand(t, fixture.plan, started)
			}
			assertNoTaskViews(t, temporary)
		})
	}
}

// assertHelperRefusalSealed proves a refused helper binding sealed its exact
// cause as a start failure without starting the provider.
func assertHelperRefusalSealed(t *testing.T, fixture binderTaskFixture, result Result, started *exec.Cmd, want error) {
	t.Helper()
	assertPendingRuntimeRejected(t, fixture.td, result)
	exit, err := os.ReadFile(filepath.Join(fixture.td.Dir, "provider.exit"))
	require(t, err)
	var seal task.ProviderExitRecord
	require(t, task.DecodeStrict(exit, &seal))
	if started != nil || seal.Error != want.Error() {
		t.Fatalf("helper refusal started=%v sealed %q, want no Start and %q", started != nil, seal.Error, want)
	}
}

// assertDirectProviderCommand proves the started command is the installed
// executable itself, with the exact plan arguments, directory and environment.
func assertDirectProviderCommand(t *testing.T, plan Plan, started *exec.Cmd) {
	t.Helper()
	if started == nil || started.Path != plan.Executable || started.Args[0] != plan.Executable ||
		!slices.Equal(started.Args[1:], plan.Arguments) || started.Dir != plan.Directory || !slices.Equal(started.Env, plan.Environment) {
		t.Fatalf("provider command is not the installed executable with exact arguments and environment: %+v", started)
	}
}
