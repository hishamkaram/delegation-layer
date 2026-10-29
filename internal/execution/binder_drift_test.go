package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

var (
	errBinderCleanup        = errors.New("supplied binder cleanup failed")
	errChangedProviderBound = errors.New("changed provider was still bound")
)

// driftProvider is a task whose provider is a private script, so a test may
// rewrite, replace or remove the admitted bytes without touching the test
// binary. refreshed is the replacement plan a fresh runtime preflight selects.
type driftProvider struct {
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

func writeProviderScript(t *testing.T, path, output string) string {
	t.Helper()
	data := []byte("#!/bin/sh\nprintf '%s\\n' '" + output + "'\n")
	require(t, os.WriteFile(path, data, 0o700))
	return task.ComputeSHA256(data)
}

func driftProviderTask(t *testing.T) driftProvider {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	admitted := filepath.Join(root, "provider")
	digest := writeProviderScript(t, admitted, "admitted")
	require(t, os.Mkdir(filepath.Join(root, "next"), 0o700))
	replacement := filepath.Join(root, "next", "provider")
	replacementDigest := writeProviderScript(t, replacement, "replacement")
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", func(meta *task.MetaRecord, plan *Plan) {
		meta.ProviderExecutable = admitted
		plan.Executable, plan.ExecutableSHA256, plan.Arguments = admitted, digest, nil
	})
	plan.Environment = append(slices.Clone(plan.Environment), "TMPDIR="+temporary)
	refreshed := plan
	refreshed.Executable, refreshed.ExecutableSHA256 = replacement, replacementDigest
	refreshed.ProviderRuntime = task.ProviderRuntimeIdentity{Executable: replacement, Version: "provider-refresh-v2", SHA256: replacementDigest}
	return driftProvider{binderTaskFixture: binderTaskFixture{td: td, permit: permit, plan: plan}, temporary: temporary, admitted: admitted, refreshed: refreshed}
}

type driftRun struct {
	result     Result
	preflights int
	starts     int
}

// run launches the task once. A nil binder selects the private copy; refresh
// installs a runtime preflight that returns the replacement plan.
func (p driftProvider) run(binder *recordingBinder, refresh bool) driftRun {
	run := driftRun{}
	options := Options{Hooks: Hooks{Start: func(cmd *exec.Cmd) error {
		run.starts++
		return cmd.Start()
	}}}
	if binder != nil {
		options.Binder = binder
	}
	if refresh {
		options.PreflightPlan = func(PreflightScope, Plan) (Plan, error) {
			run.preflights++
			return p.refreshed, nil
		}
	}
	run.result = Run(p.td, p.permit, p.plan, options)
	return run
}

// assertReplacementLaunched proves one preflight and one Start of the
// replacement, with the replacement identity in the start and exit records.
func (p driftProvider) assertReplacementLaunched(t *testing.T, run driftRun) {
	t.Helper()
	assertSuccess(t, run.result)
	if run.preflights != 1 || run.starts != 1 {
		t.Fatalf("preflight=%d start=%d, want one each", run.preflights, run.starts)
	}
	if got := string(payload(t, p.td, run.result.Outcome)); got != "replacement\n" {
		t.Fatalf("provider output %q, want the replacement", got)
	}
	assertObservedRuntimeEvidence(t, p.td, p.refreshed.ProviderRuntime)
	assertNoTaskViews(t, p.temporary)
}

// assertRefused proves a sealed start refusal with diagnostic want, no
// preflight and no Start.
func (p driftProvider) assertRefused(t *testing.T, run driftRun, want string) {
	t.Helper()
	assertPendingRuntimeRejected(t, p.td, run.result)
	if run.preflights != 0 || run.starts != 0 {
		t.Fatalf("refusal ran preflight=%d start=%d", run.preflights, run.starts)
	}
	exit, err := os.ReadFile(filepath.Join(p.td.Dir, "provider.exit"))
	require(t, err)
	var seal task.ProviderExitRecord
	require(t, task.DecodeStrict(exit, &seal))
	if seal.Error != want {
		t.Fatalf("refusal sealed %q, want %q", seal.Error, want)
	}
	assertNoTaskViews(t, p.temporary)
}

// admittedChange is the verifiedexec owner's own report for a changed
// admitted executable, not one fabricated by the test.
func admittedChange(request verifiedexec.CommandRequest) error {
	command, err := verifiedexec.NewCommandFromRequest(request)
	if err == nil {
		return errors.Join(errChangedProviderBound, command.Close())
	}
	return err
}

func TestInitialBindingDefersQueuedProviderChangeToPreflight(t *testing.T) {
	for _, change := range queuedProviderChanges {
		for _, supplied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/supplied=%t", change.name, supplied), func(t *testing.T) {
				provider := driftProviderTask(t)
				var binder *recordingBinder
				if supplied {
					binder = &recordingBinder{}
				}
				change.apply(t, provider.admitted)
				provider.assertReplacementLaunched(t, provider.run(binder, true))
				if !supplied {
					return
				}
				if binder.calls() != 2 || binder.requests[0].Executable != provider.admitted ||
					binder.requests[1].Executable != provider.refreshed.Executable || binder.requests[1].SHA256 != provider.refreshed.ExecutableSHA256 {
					t.Fatalf("supplied binder did not bind the admitted then the replacement plan: %+v", binder.requests)
				}
			})
		}
	}
}

func TestInitialBindingDefersSingleWrappedProviderChange(t *testing.T) {
	provider := driftProviderTask(t)
	writeProviderScript(t, provider.admitted, "rewritten")
	var reported error
	binder := &recordingBinder{initial: func(request verifiedexec.CommandRequest) error {
		reported = fmt.Errorf("pool binder: %w", admittedChange(request))
		return reported
	}}
	provider.assertReplacementLaunched(t, provider.run(binder, true))
	if !errors.Is(reported, verifiedexec.ErrAdmittedExecutableChanged) || binder.calls() != 2 {
		t.Fatalf("reported=%v calls=%d, want a wrapped admitted change then one refreshed binding", reported, binder.calls())
	}
}

func TestInitialBindingRefusesUnrelatedBinderRefusalDespiteChange(t *testing.T) {
	for _, refusal := range []error{errRecordingBinder, fmt.Errorf("pool gate: %w", context.Canceled)} {
		t.Run(refusal.Error(), func(t *testing.T) {
			provider := driftProviderTask(t)
			binder := &recordingBinder{initial: func(request verifiedexec.CommandRequest) error {
				writeProviderScript(t, request.Executable, "rewritten")
				return refusal
			}}
			provider.assertRefused(t, provider.run(binder, true), refusal.Error())
			if binder.calls() != 1 {
				t.Fatalf("binder calls = %d, want only the refused initial binding", binder.calls())
			}
		})
	}
}

func TestInitialBindingRefusesJoinedProviderChangeFault(t *testing.T) {
	provider := driftProviderTask(t)
	writeProviderScript(t, provider.admitted, "rewritten")
	var change, reported error
	binder := &recordingBinder{initial: func(request verifiedexec.CommandRequest) error {
		change = admittedChange(request)
		reported = errors.Join(change, errBinderCleanup)
		return reported
	}}
	run := provider.run(binder, true)
	if !errors.Is(change, verifiedexec.ErrAdmittedExecutableChanged) {
		t.Fatalf("fixture did not observe an admitted change: %v", change)
	}
	provider.assertRefused(t, run, reported.Error())
	if binder.calls() != 1 {
		t.Fatalf("binder calls = %d, want only the refused initial binding", binder.calls())
	}
}

func TestDisabledRefreshKeepsProviderChangeRefusal(t *testing.T) {
	for _, change := range queuedProviderChanges[:2] {
		for _, supplied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/supplied=%t", change.name, supplied), func(t *testing.T) {
				provider := driftProviderTask(t)
				var binder *recordingBinder
				if supplied {
					binder = &recordingBinder{}
				}
				change.apply(t, provider.admitted)
				provider.assertRefused(t, provider.run(binder, false), "runtime executable identity does not match admission")
			})
		}
	}
}

// An untagged fault of the admitted path refuses even though the path now
// holds other bytes: only the owner's admitted-change report may defer.
func TestInitialBindingRefusesUntaggedProviderFaultDespiteChange(t *testing.T) {
	for _, change := range []providerChange{
		{"symlink", func(t *testing.T, path string) {
			linked := filepath.Join(filepath.Dir(path), "linked")
			writeProviderScript(t, linked, "linked")
			require(t, errors.Join(os.Remove(path), os.Symlink(linked, path)))
		}},
		{"not-executable", func(t *testing.T, path string) {
			writeProviderScript(t, path, "rewritten")
			require(t, os.Chmod(path, 0o600))
		}},
	} {
		for _, supplied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/supplied=%t", change.name, supplied), func(t *testing.T) {
				provider := driftProviderTask(t)
				var binder *recordingBinder
				if supplied {
					binder = &recordingBinder{}
				}
				change.apply(t, provider.admitted)
				_, fault := verifiedexec.Open(provider.admitted, provider.plan.ExecutableSHA256)
				if fault == nil || errors.Is(fault, verifiedexec.ErrAdmittedExecutableChanged) {
					t.Fatalf("fixture fault = %v, want an untagged refusal", fault)
				}
				provider.assertRefused(t, provider.run(binder, true), fault.Error())
			})
		}
	}
}
