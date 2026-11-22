package probe

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
)

type modelFinishFaultLifetime struct{}

func (modelFinishFaultLifetime) StartCommand(_ *exec.Cmd, start func() error) error { return start() }

func (modelFinishFaultLifetime) FinishCommand(context.Context, *exec.Cmd) error {
	return errors.New("project custody lost after command wait")
}
func (modelFinishFaultLifetime) Terminate(context.Context) error { return nil }
func (modelFinishFaultLifetime) CloseTask(context.Context) error { return nil }

func TestModelCaptureRejectsLifetimeFinishFailure(t *testing.T) {
	var captured modelCommandResult
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{
		Stopper: runtimeTestStopper{}, Lifetime: modelFinishFaultLifetime{},
	}, func(scope execution.PreflightScope) error {
		var err error
		captured, err = runModelCommand(scope, testModelDefinition(t, 128), nil, nil, nativeHooks{
			start: func(cmd *exec.Cmd) error {
				_, writeErr := io.WriteString(cmd.Stdout, "valid model catalog")
				return writeErr
			},
			wait: func(*exec.Cmd) error { return nil },
		})
		return err
	})
	if !errors.Is(workErr, errNativeInspection) || stopErr != nil || len(captured.stdout) != 0 {
		t.Fatalf("lifetime failure became advisory model output: result=%+v work=%v stop=%v", captured, workErr, stopErr)
	}
}
