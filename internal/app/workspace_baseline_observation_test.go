package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// baselineGitShim is the PATH entry for production baseline Git. It runs
// inside the task's supervised launches, so it observes Git state while the
// guardian is live without an independent query. It runs the real Git for
// each observation and renames the complete output into place before it
// returns or announces readiness; partial output never has the final name.
type baselineGitShim struct{ bin, observations string }

const baselineGitShimScript = `#!/bin/sh
real=$DELEGATE_BASELINE_REAL_GIT
out=$DELEGATE_BASELINE_OBSERVATIONS
ref=$DELEGATE_BASELINE_REF
if [ $# -eq 7 ] && [ "$1" = -C ] && [ "$3" = update-ref ] && [ "$4" = --no-deref ] && [ "$5" = "$ref" ]; then
  "$real" "$@" || exit
  "$real" -C "$2" --no-replace-objects for-each-ref --format='%(objectname)' "$ref" > "$out/pin-ref.partial" &&
    "$real" -C "$2" --no-replace-objects cat-file blob "$ref:lead.txt" > "$out/pin-lead.partial" &&
    mv "$out/pin-lead.partial" "$out/pin-lead" && mv "$out/pin-ref.partial" "$out/pin-ref" || exit 93
  exit 0
fi
if [ -n "$DELEGATE_BASELINE_HOLD_FIFO" ] && [ "$1" = -C ] && [ "$3" = add ] && [ "$4" = --sparse ] && [ "$5" = -A ]; then
  "$real" -C "$2" --no-replace-objects for-each-ref --format='%(refname)' "$ref" > "$out/held-ref.partial" &&
    mv "$out/held-ref.partial" "$out/held-ref" || exit 94
  printf ready > "$DELEGATE_BASELINE_READY_FIFO"
  IFS= read -r token < "$DELEGATE_BASELINE_HOLD_FIFO"
  exit 91
fi
exec "$real" "$@"
`

// installBaselineGitShim observes ref, the turn's exact baseline ref, at
// production PinTurnBaseline's create-only update-ref argv.
func installBaselineGitShim(t *testing.T, root, ref string) baselineGitShim {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := baselineGitShim{bin: filepath.Join(root, "git-shim"), observations: filepath.Join(root, "git-observations")}
	for _, directory := range []string{shim.bin, shim.observations} {
		if err = os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(shim.bin, "git"), []byte(baselineGitShimScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELEGATE_BASELINE_REAL_GIT", realGit)
	t.Setenv("DELEGATE_BASELINE_OBSERVATIONS", shim.observations)
	t.Setenv("DELEGATE_BASELINE_REF", ref)
	t.Setenv("PATH", shim.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return shim
}

// pinned reads, in process, what the shim observed after production pinned
// baseline: the raw ref names the journal tree, whose lead.txt holds lead.
func (s baselineGitShim) pinned(baseline workspace.TurnBaseline, lead string) error {
	ref, err := os.ReadFile(filepath.Join(s.observations, "pin-ref"))
	if err != nil {
		return fmt.Errorf("baseline pin was not observed before provider Start: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(s.observations, "pin-lead"))
	if err != nil {
		return fmt.Errorf("baseline pin observation is incomplete: %w", err)
	}
	if string(ref) != baseline.TreeOID+"\n" || string(content) != lead {
		return fmt.Errorf("baseline is not pinned with the Lead edit: ref=%q lead=%q, want tree %s", ref, content, baseline.TreeOID)
	}
	return nil
}

// heldRefAbsent reads the ref query the shim completed before it announced
// the held capture. A missing record is a failed observation, not absence.
func (s baselineGitShim) heldRefAbsent() error {
	refs, err := os.ReadFile(filepath.Join(s.observations, "held-ref"))
	if err != nil {
		return fmt.Errorf("held baseline Git did not record its ref query: %w", err)
	}
	if len(refs) != 0 {
		return fmt.Errorf("baseline ref was pinned before the held capture: %q", refs)
	}
	return nil
}

// installBaselineGitBarrier holds the shim's baseline capture `add` until
// the returned hold FIFO is written; the ready FIFO announces entry.
func installBaselineGitBarrier(t *testing.T, shim baselineGitShim) (*os.File, *os.File) {
	t.Helper()
	ready := baselineFIFO(t, filepath.Join(shim.bin, "ready"))
	hold := baselineFIFO(t, filepath.Join(shim.bin, "hold"))
	t.Setenv("DELEGATE_BASELINE_READY_FIFO", ready.Name())
	t.Setenv("DELEGATE_BASELINE_HOLD_FIFO", hold.Name())
	return ready, hold
}

func baselineFIFO(t *testing.T, path string) *os.File {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, file.Close()) })
	return file
}

func TestBaselineGitShimObservationsRequireCompleteRecords(t *testing.T) {
	baseline := workspace.TurnBaseline{TreeOID: "1111111111111111111111111111111111111111"}
	cases := map[string]struct {
		files map[string]string
		check func(baselineGitShim) error
	}{
		"missing pin":        {check: func(s baselineGitShim) error { return s.pinned(baseline, "lead\n") }},
		"partial pin":        {files: map[string]string{"pin-ref": baseline.TreeOID + "\n", "pin-lead.partial": "lead\n"}, check: func(s baselineGitShim) error { return s.pinned(baseline, "lead\n") }},
		"other tree":         {files: map[string]string{"pin-ref": "2222222222222222222222222222222222222222\n", "pin-lead": "lead\n"}, check: func(s baselineGitShim) error { return s.pinned(baseline, "lead\n") }},
		"other lead":         {files: map[string]string{"pin-ref": baseline.TreeOID + "\n", "pin-lead": "other\n"}, check: func(s baselineGitShim) error { return s.pinned(baseline, "lead\n") }},
		"missing held query": {files: map[string]string{"held-ref.partial": ""}, check: baselineGitShim.heldRefAbsent},
		"held ref present":   {files: map[string]string{"held-ref": "refs/delegate/pinned\n"}, check: baselineGitShim.heldRefAbsent},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			shim := baselineGitShim{observations: t.TempDir()}
			for file, content := range test.files {
				writeLineageFile(t, filepath.Join(shim.observations, file), content)
			}
			if err := test.check(shim); err == nil {
				t.Fatal("accepted a missing, partial or wrong shim observation")
			}
		})
	}
	complete := baselineGitShim{observations: t.TempDir()}
	writeLineageFile(t, filepath.Join(complete.observations, "pin-ref"), baseline.TreeOID+"\n")
	writeLineageFile(t, filepath.Join(complete.observations, "pin-lead"), "lead\n")
	writeLineageFile(t, filepath.Join(complete.observations, "held-ref"), "")
	if err := errors.Join(complete.pinned(baseline, "lead\n"), complete.heldRefAbsent()); err != nil {
		t.Fatalf("complete shim observations: %v", err)
	}
}

// A lineage query while the guardian owns the test process is refused before
// any command or native scope starts, through every lineage launch path. The
// task's own launch then completes normally, the guardian joins its finite
// worker's ordinary exit, and lineage queries work again once it has joined.
func TestNativeBaselineGuardianRefusesIndependentLineageQuery(t *testing.T) {
	fixture := newNativeBaselineTask(t)
	owner := fixture.guardian(t, time.Minute)
	direct := exec.Command("git", "-C", fixture.lineage.main, "rev-parse", "HEAD")
	if err := fixture.lineage.launch(direct); !errors.Is(err, errLiveFixtureOwner) || direct.Process != nil {
		t.Fatalf("live-owner launch boundary: process=%v err=%v", direct.Process, err)
	}
	viaRunner := exec.Command("git", "-C", fixture.lineage.main, "rev-parse", "HEAD")
	if err := fixture.lineage.git.Runner.Run(fixture.lineage.scope.ctx, viaRunner); !errors.Is(err, errLiveFixtureOwner) || viaRunner.Process != nil {
		t.Fatalf("live-owner workspace Git runner: process=%v err=%v", viaRunner.Process, err)
	}
	if succeeded, err := fixture.lineage.gitExitsZero(fixture.lineage.main, "rev-parse", "HEAD"); !errors.Is(err, errLiveFixtureOwner) || succeeded {
		t.Fatalf("live-owner exit query became an ordinary result: succeeded=%v err=%v", succeeded, err)
	}
	result := fixture.run(owner.child, fixture.dependencies())
	if result.code != 0 || result.err != nil || result.response.Publication != task.PublicationCommitted.String() || fixture.starts.Load() != 1 {
		t.Fatalf("managed launch after refused queries: code=%d err=%v starts=%d response=%+v", result.code, result.err, fixture.starts.Load(), result.response)
	}
	if err := owner.finish(t); err != nil {
		t.Fatalf("guardian after refused queries: %v", err)
	}
	assertBaselineWorkerExited(t, fixture.td)
	if head := fixture.lineage.gitIn(t, fixture.registration.CheckoutPath, "rev-parse", "HEAD"); head != fixture.registration.BaseOID {
		t.Fatalf("lineage query after the guardian joined: HEAD %q, want %s", head, fixture.registration.BaseOID)
	}
}

// The real shim's held branch must announce readiness only after its ref query
// recorded successfully. A held file stands in for the hold FIFO, so every
// case is finite; the lineage's native scope owns each Start and Wait.
func TestBaselineGitShimHeldQueryFaultsWithholdReadiness(t *testing.T) {
	lineage := newNativeLineage(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(lineage.root, "git-shim")
	if err = os.Mkdir(shim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(shim, "git"), []byte(baselineGitShimScript), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		directory string
		prepare   func(t *testing.T, observations string)
		exit      int
	}{
		"query succeeds":    {directory: lineage.main, exit: 91},
		"Git query fails":   {directory: filepath.Join(lineage.root, "absent"), exit: 94},
		"record move fails": {directory: lineage.main, exit: 94, prepare: blockBaselineHeldRecord},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			observations, ready, hold := t.TempDir(), filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "hold")
			writeLineageFile(t, hold, "release\n")
			if test.prepare != nil {
				test.prepare(t, observations)
			}
			command := exec.Command(filepath.Join(shim, "git"), "-C", test.directory, "add", "--sparse", "-A")
			command.Env = append(os.Environ(), "DELEGATE_BASELINE_REAL_GIT="+realGit, "DELEGATE_BASELINE_OBSERVATIONS="+observations,
				"DELEGATE_BASELINE_REF=refs/delegate/fixture/baselines/1", "DELEGATE_BASELINE_READY_FIFO="+ready, "DELEGATE_BASELINE_HOLD_FIFO="+hold)
			runErr := lineage.launch(command)
			assertBaselineHeldReadiness(t, baselineGitShim{observations: observations}, ready, test.exit == 91)
			if command.ProcessState == nil || command.ProcessState.ExitCode() != test.exit || !ordinaryGitExit(runErr) {
				t.Fatalf("shim joined exit = %v, want %d: %v", command.ProcessState, test.exit, runErr)
			}
		})
	}
}

// A nonempty directory at the moved name's target makes only the record move
// fail; the final name is then a directory, never an absent-ref record.
func blockBaselineHeldRecord(t *testing.T, observations string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(observations, "held-ref", "held-ref.partial", "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// A held query announces readiness and records absence only when it succeeded;
// otherwise neither the marker nor an absent-ref record exists.
func assertBaselineHeldReadiness(t *testing.T, shim baselineGitShim, ready string, succeeded bool) {
	t.Helper()
	announced, readErr := os.ReadFile(ready)
	held := shim.heldRefAbsent()
	if succeeded && (string(announced) != "ready" || held != nil) {
		t.Fatalf("successful held query: ready=%q,%v held=%v", announced, readErr, held)
	}
	if !succeeded && (!errors.Is(readErr, os.ErrNotExist) || held == nil) {
		t.Fatalf("failed held query announced readiness or absence: ready=%q,%v held=%v", announced, readErr, held)
	}
}
