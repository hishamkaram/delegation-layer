package execution

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func configureLaunchFiles(meta *task.MetaRecord, plan *Plan) {
	plan.Arguments = append(plan.Arguments, "", "")
	plan.InputFiles = []task.InputFile{{Name: "settings.json", ArgumentIndex: 1, Content: `{"safe":true}`}}
	plan.OutputArtifacts = []task.OutputArtifact{{Name: "answer.txt", ArgumentIndex: 2}}
	plan.OutputWriterContract = task.OutputWriterProcessExitEOF
	meta.InputFiles = plan.InputFiles
	meta.OutputArtifacts = plan.OutputArtifacts
	meta.OutputWriterContract = plan.OutputWriterContract
}

func TestLaunchFilesBindBeforeStartAndImportAfterCapture(t *testing.T) {
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configureLaunchFiles)
	captureEntered := make(chan struct{})
	captureRelease := make(chan struct{})
	var once sync.Once
	startFailure := errors.New("injected start failure")
	opts := Options{Hooks: Hooks{
		Start: func(cmd *exec.Cmd) error {
			input, err := os.ReadFile(cmd.Args[2])
			if err != nil || !bytes.Equal(input, []byte(plan.InputFiles[0].Content)) {
				return errors.New("input was not materialized before start")
			}
			if err := os.WriteFile(cmd.Args[3], []byte("captured artifact"), 0o600); err != nil {
				return err
			}
			return startFailure
		},
		Capture: func(_ string, dst io.Writer, src io.Reader) error {
			once.Do(func() { close(captureEntered) })
			<-captureRelease
			_, err := io.Copy(dst, src)
			return err
		},
	}}
	completed := make(chan Result, 1)
	go func() { completed <- Run(td, permit, plan, opts) }()
	select {
	case <-captureEntered:
	case early := <-completed:
		t.Fatalf("execution ended before capture: %+v", early)
	}
	for _, path := range []string{"provider.exit", "raw/answer.txt"} {
		if _, err := os.Stat(filepath.Join(td.Dir, path)); !errors.Is(err, os.ErrNotExist) {
			close(captureRelease)
			<-completed
			t.Fatalf("%s existed before capture EOF: %v", path, err)
		}
	}
	close(captureRelease)
	result := <-completed
	require(t, errors.Join(result.Error, result.CleanupError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("start failure was not rejected: %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(td.Dir, "raw", "answer.txt"))
	require(t, err)
	if string(content) != "captured artifact" {
		t.Fatalf("wrong imported bytes: %q", content)
	}
	if plan.Arguments[1] != "" || plan.Arguments[2] != "" {
		t.Fatal("execution mutated the provider's argv declaration")
	}
}

func TestLaunchFilesRejectChangedDeclarationWithoutConsumingPermit(t *testing.T) {
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configureLaunchFiles)
	plan.InputFiles = []task.InputFile{{Name: "settings.json", ArgumentIndex: 1, Content: "changed"}}
	result := Run(td, permit, plan, Options{})
	if !errors.Is(result.Error, task.ErrIdentityMismatch) || permit.IsConsumed() {
		t.Fatalf("changed content gained launch authority: %+v", result)
	}
}

func TestLaunchFilesStartFailureWithAbsentOutputStillSeals(t *testing.T) {
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configureLaunchFiles)
	result := Run(td, permit, plan, Options{Hooks: Hooks{Start: func(*exec.Cmd) error {
		return errors.New("no child started")
	}}})
	require(t, errors.Join(result.Error, result.CleanupError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("missing rejected outcome: %+v", result)
	}
	seal, err := os.ReadFile(filepath.Join(td.Dir, "provider.exit"))
	require(t, err)
	if strings.Contains(string(seal), "raw/answer.txt") {
		t.Fatal("absent output fabricated a manifest entry")
	}
}
