package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// schedulerHooks owns only this bounded invocation and its temporary input.
// The scheduler store owns the durable launch claim and notification result.
type schedulerHooks struct{ dependencies Dependencies }

func (hooks schedulerHooks) RunHook(ctx context.Context, hook contract.FailureHook, notice contract.Notification) (result string, resultErr error) {
	if err := contract.ValidateFailureHook(&hook); err != nil {
		return "unavailable", errors.New("invalid failure hook configuration")
	}
	data, err := schedulerHookContext(notice)
	if err != nil {
		return "unavailable", err
	}
	if info, statErr := os.Stat(hook.Executable); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "unavailable", nil
	}
	if !filepath.IsAbs(hooks.dependencies.RunnerExecutable) {
		return "unavailable", errors.New("failure hook requires the selected installed runner")
	}
	input, err := os.CreateTemp("", "delegate-schedule-hook-*")
	if err != nil {
		return "unavailable", errors.New("failure hook context creation failed")
	}
	defer func() {
		if closeErr := input.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("failure hook context close failed"))
		}
		if removeErr := os.Remove(input.Name()); removeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("failure hook context removal failed"))
		}
	}()
	if _, err = input.Write(data); err != nil {
		return "unavailable", errors.New("failure hook context write failed")
	}
	if _, err = input.Seek(0, 0); err != nil {
		return "unavailable", errors.New("failure hook context rewind failed")
	}
	command := exec.Command(hook.Executable)
	command.Dir = filepath.Dir(hook.Executable)
	command.Env = schedulerHookEnvironment(hooks.dependencies.Environment)
	command.Stdin = input
	bounded, cancel := context.WithTimeout(ctx, hook.Timeout)
	defer cancel()
	err = execution.RunIsolatedCommandNullOutput(bounded, hooks.dependencies.RunnerExecutable, command)
	if err == nil {
		return "succeeded", nil
	}
	if execution.NativeCommandTimedOut(err) {
		return "timed_out", nil
	}
	if _, exited := execution.NativeCommandExitCode(err); exited {
		return "failed", nil
	}
	// Helper or cleanup loss does not prove whether the callback completed.
	return "uncertain", nil
}

func schedulerHookContext(notice contract.Notification) ([]byte, error) {
	for _, id := range []string{notice.EventID, notice.ScheduleID, notice.AnchorTaskID, notice.LastTaskID} {
		if err := task.ValidateTaskID(id); err != nil {
			return nil, errors.New("invalid failure hook notification identity")
		}
	}
	if notice.Kind != "failure" && notice.Kind != "recovery" {
		return nil, errors.New("invalid failure hook notification kind")
	}
	if notice.StreakCount < 0 {
		return nil, errors.New("invalid failure hook notification context")
	}
	if notice.FailureCode != "" {
		if err := task.ValidateOutputDiagnostic(task.OutputDiagnostic{Code: notice.FailureCode, Message: "Schedule result was not committed."}); err != nil {
			return nil, errors.New("invalid failure hook diagnostic code")
		}
	}
	// Only these bounded fields cross into a user callback; no paths, messages,
	// credentials or provider output are added to its context.
	contextRecord := struct {
		EventID     string `json:"event_id"`
		ScheduleID  string `json:"schedule_id"`
		Kind        string `json:"kind"`
		StreakCount int64  `json:"streak_count"`
		FailureCode string `json:"failure_code,omitempty"`
		LastTaskID  string `json:"last_task_id"`
	}{notice.EventID, notice.ScheduleID, notice.Kind, notice.StreakCount, notice.FailureCode, notice.LastTaskID}
	data, err := json.Marshal(contextRecord)
	if err != nil || len(data) > 4096 {
		return nil, errors.New("failure hook context exceeds its bound")
	}
	return data, nil
}

func schedulerHookEnvironment(environment []string) []string {
	selected := make([]string, 0)
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if ok && (name == "PATH" || name == "HOME" || name == "TMPDIR" || name == "LANG" || strings.HasPrefix(name, "LC_")) {
			selected = append(selected, entry)
		}
	}
	return selected
}
