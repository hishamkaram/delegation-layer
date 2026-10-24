package execution

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	environmentProbeMode     = "environment-probe"
	helperModeArgumentPrefix = "delegate-helper-mode="
	environmentMarkerName    = "DELEGATE_TEST_INHERITED_MARKER"
	ambientMarker            = "ambient-marker"
	selectedMarker           = "selected-marker"
)

// helperMode reads the child's mode from the recorded environment, then from a
// positional argument for launches whose environment is deliberately empty.
func helperMode() string {
	if mode := os.Getenv("DELEGATE_EXECUTION_HELPER"); mode != "" {
		return mode
	}
	for _, argument := range os.Args[1:] {
		if mode, found := strings.CutPrefix(argument, helperModeArgumentPrefix); found {
			return mode
		}
	}
	return ""
}

// writeEnvironmentProbe reports only the synthetic marker and PWD, never the
// child environment as a whole.
func writeEnvironmentProbe(out io.Writer) error {
	_, err := fmt.Fprintf(out, "marker=%s\npwd=%s\n", probeValue(environmentMarkerName), probeValue("PWD"))
	return err
}

func probeValue(name string) string {
	if value, found := os.LookupEnv(name); found {
		return value
	}
	return "unset"
}

func marker(value string) string { return environmentMarkerName + "=" + value }

func configureEnvironmentProbe(environment []string) func(*task.MetaRecord, *Plan) {
	return func(_ *task.MetaRecord, plan *Plan) {
		plan.Arguments = append(plan.Arguments, helperModeArgumentPrefix+environmentProbeMode)
		plan.Environment = environment
	}
}

func runEnvironmentProbe(t *testing.T, configure func(*task.MetaRecord, *Plan), opts Options) (Plan, string) {
	t.Helper()
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configure)
	result := Run(td, permit, plan, opts)
	assertSuccess(t, result)
	return plan, string(payload(t, td, result.Outcome))
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dl-env-")
	require(t, err)
	t.Cleanup(func() { require(t, os.RemoveAll(dir)) })
	canonical, err := filepath.EvalSymlinks(dir)
	require(t, err)
	return canonical
}

func TestLaunchEnvironmentSelection(t *testing.T) {
	cases := []struct {
		name       string
		plan       []string
		inherited  []string
		wantMarker string
		pwdAdded   bool
	}{
		{"nil plan with empty selection inherits nothing", nil, []string{}, "unset", true},
		{"nil plan uses supplied selection", nil, []string{marker(selectedMarker)}, selectedMarker, true},
		{"explicit empty plan overrides non-empty selection", []string{}, []string{marker(selectedMarker)}, "unset", false},
		{"nil plan and nil option keep the standalone process default", nil, nil, ambientMarker, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(environmentMarkerName, ambientMarker)
			supplied := slices.Clone(tc.inherited)
			plan, got := runEnvironmentProbe(t, configureEnvironmentProbe(tc.plan), Options{InheritedEnvironment: tc.inherited})
			wantPWD := "unset"
			if tc.pwdAdded {
				wantPWD = plan.Directory
			}
			if want := fmt.Sprintf("marker=%s\npwd=%s\n", tc.wantMarker, wantPWD); got != want {
				t.Fatalf("child observed %q, want %q", got, want)
			}
			if !slices.Equal(supplied, tc.inherited) || (supplied == nil) != (tc.inherited == nil) {
				t.Fatalf("Run mutated the supplied inherited environment: %v -> %v", supplied, tc.inherited)
			}
		})
	}
}

func TestNilPlanSuppliedSelectionResolvesInterpreterAndChildEnvironment(t *testing.T) {
	t.Setenv(environmentMarkerName, ambientMarker)
	root := shortTempDir(t)
	binDir := filepath.Join(root, "bin")
	require(t, os.Mkdir(binDir, 0o700))
	shell, err := exec.LookPath("sh")
	require(t, err)
	shell, err = filepath.EvalSymlinks(shell)
	require(t, err)
	require(t, os.Symlink(shell, filepath.Join(binDir, "sh")))
	script := filepath.Join(root, "provider")
	content := []byte("#!/usr/bin/env sh\nprintf 'marker=%s path=%s\\n' \"$" + environmentMarkerName + "\" \"$PATH\"\n")
	require(t, os.WriteFile(script, content, 0o700))
	selection := []string{"PATH=" + binDir, marker(selectedMarker)}
	_, got := runEnvironmentProbe(t, func(meta *task.MetaRecord, plan *Plan) {
		meta.ProviderExecutable = script
		plan.Executable, plan.ExecutableSHA256, plan.Arguments, plan.Environment = script, task.ComputeSHA256(content), nil, nil
	}, Options{InheritedEnvironment: selection})
	if want := fmt.Sprintf("marker=%s path=%s\n", selectedMarker, binDir); got != want {
		t.Fatalf("script observed %q, want %q", got, want)
	}
}

func TestPreflightRefreshKeepsSelectedInheritedScope(t *testing.T) {
	t.Setenv(environmentMarkerName, ambientMarker)
	selection := []string{marker(selectedMarker)}
	opts := Options{InheritedEnvironment: selection}
	opts.PreflightPlan = func(_ PreflightScope, admitted Plan) (Plan, error) {
		t.Setenv(environmentMarkerName, "changed-"+ambientMarker)
		selection[0] = marker("mutated-" + selectedMarker)
		return finalizedRuntimePlan(admitted), nil
	}
	plan, got := runEnvironmentProbe(t, func(meta *task.MetaRecord, plan *Plan) {
		configurePendingRuntimePreflight(meta, plan)
		configureEnvironmentProbe(nil)(meta, plan)
	}, opts)
	if want := fmt.Sprintf("marker=%s\npwd=%s\n", selectedMarker, plan.Directory); got != want {
		t.Fatalf("refreshed launch observed %q, want %q", got, want)
	}
}

func TestInheritedCommandEnvironmentCopiesCaptureAndAppendsImplicitEntries(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "work") + string(filepath.Separator)
	for _, captured := range [][]string{nil, {}, {marker(selectedMarker), "PWD=/stale"}} {
		supplied := slices.Clone(captured)
		got := InheritedCommandEnvironment(captured, directory)
		want := append(slices.Clone(supplied), implicitWorkingDirectoryEntries(directory)...)
		if got == nil || !slices.Equal(got, want) {
			t.Fatalf("environment for %q = %q, want %q", captured, got, want)
		}
		if len(captured) != 0 {
			got[0] = "MUTATED=1"
			if !slices.Equal(captured, supplied) {
				t.Fatalf("result aliased the captured environment: %q", captured)
			}
		}
	}
}
