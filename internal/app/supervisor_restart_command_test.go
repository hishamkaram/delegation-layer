package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSupervisorRuntimeRestartAcceptedPendingPreservesCause(t *testing.T) {
	from := infrastructure.RuntimeRestartReference{RootID: strings.Repeat("a", 32), Startup: infrastructure.StartupReference{
		Sequence: 1, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64),
	}}
	report := infrastructure.RuntimeRestartReport{From: from.Startup, Phase: infrastructure.RuntimeRestartShutdownPending}
	for _, observation := range []error{context.DeadlineExceeded, context.Canceled, pueue.ErrInFlight} {
		t.Run(observation.Error(), func(t *testing.T) {
			const private = "/private/pending-observation"
			cause := errors.Join(fmt.Errorf("%s: %w", private, pueue.ErrPrivateRuntimeRestartPending), infrastructure.ErrOwnerPending, observation)
			token, err := infrastructure.FormatRuntimeRestartReference(from)
			if err != nil {
				t.Fatal(err)
			}
			response := newSupervisorCommandResponse("restart")
			response.SupervisorControl.RuntimeRestart = &SupervisorRuntimeRestartResponse{FromStartup: token}
			result := finishRuntimeRestart(response, from, report, cause)
			if result.code != 3 || result.response.SupervisorControl.State != "pending" || result.response.SupervisorControl.NextAction != "repeat_same_restart" || result.response.SupervisorControl.RuntimeRestart.FromStartup != token {
				t.Fatalf("accepted pending became a fault or changed its original operation: exit%d %+v", result.code, result.response.SupervisorControl)
			}
			if result.err == nil || !errors.Is(result.err, observation) || !errors.Is(result.err, pueue.ErrPrivateRuntimeRestartPending) || strings.Contains(result.err.Error(), private) {
				t.Fatalf("pending presentation discarded its cause or exposed a private path: %v", result.err)
			}
		})
	}
}

type runtimeRestartRetainedTestError struct{ cause error }

func (e *runtimeRestartRetainedTestError) Error() string         { return e.cause.Error() }
func (e *runtimeRestartRetainedTestError) Unwrap() error         { return e.cause }
func (*runtimeRestartRetainedTestError) RetainsResidentCleanup() {}

type runtimeRestartRetainedStoreTestError struct{ cause error }

func (e *runtimeRestartRetainedStoreTestError) Error() string { return e.cause.Error() }
func (e *runtimeRestartRetainedStoreTestError) Unwrap() error { return e.cause }
func (*runtimeRestartRetainedStoreTestError) RetainedStore() *taskdir.Store {
	// This pure exit-policy fixture owns no real Store or external resources.
	return &taskdir.Store{}
}

type runtimeRestartAliasTestError struct{}

func (runtimeRestartAliasTestError) Error() string { return "unknown alias" }
func (runtimeRestartAliasTestError) Is(error) bool { return true }

type runtimeRestartSliceTestError []string

func (runtimeRestartSliceTestError) Error() string { return "unknown non-comparable cause" }

func TestSupervisorRuntimeRestartPendingCannotHideFaultOrCleanup(t *testing.T) {
	from := infrastructure.RuntimeRestartReference{RootID: strings.Repeat("a", 32), Startup: infrastructure.StartupReference{
		Sequence: 1, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64),
	}}
	report := infrastructure.RuntimeRestartReport{From: from.Startup, Phase: infrastructure.RuntimeRestartShutdownPending}
	pending := errors.Join(pueue.ErrPrivateRuntimeRestartPending, infrastructure.ErrOwnerPending)
	for _, cause := range []error{
		errors.Join(pending, pueue.ErrBinding),
		errors.Join(pending, pueue.ErrUnknown),
		errors.Join(pending, errors.New("private storage fault")),
		errors.Join(pending, runtimeRestartAliasTestError{}),
		errors.Join(pending, runtimeRestartSliceTestError{"unknown"}),
		&runtimeRestartRetainedTestError{cause: pending},
		&runtimeRestartRetainedTestError{cause: supervisor.ErrRuntimeQuiescenceBusy},
		&runtimeRestartRetainedStoreTestError{cause: pending},
		&runtimeRestartRetainedStoreTestError{cause: supervisor.ErrRuntimeQuiescenceBusy},
		context.DeadlineExceeded,
	} {
		response := newSupervisorCommandResponse("restart")
		response.SupervisorControl.RuntimeRestart = &SupervisorRuntimeRestartResponse{}
		result := finishRuntimeRestart(response, from, report, cause)
		if result.code != 1 || result.err == nil || result.response.SupervisorControl.NextAction != "stop_and_report" {
			t.Errorf("fault or retained cleanup became pending/retry: cause%T exit%d %+v", cause, result.code, result.response.SupervisorControl)
		}
	}
}

func TestSupervisorRuntimeRestartPendingPreflightKeepsOriginalIntent(t *testing.T) {
	for _, custody := range []infrastructure.RuntimeRestartCustody{
		infrastructure.RuntimeRestartCustodyAvailable,
		infrastructure.RuntimeRestartCustodyUnavailable,
		infrastructure.RuntimeRestartCustodyUnknown,
	} {
		if got := runtimeRestartPreflightNextAction(custody, true); got != "repeat_original_restart" {
			t.Errorf("pending %s offered a new startup token: %s", custody, got)
		}
	}
	if got := runtimeRestartPreflightNextAction(infrastructure.RuntimeRestartCustodyAvailable, false); got != "explicit_restart" {
		t.Fatalf("unfenced custody lost explicit operator action: %s", got)
	}
	if got := runtimeRestartPreflightNextAction(infrastructure.RuntimeRestartCustodyUnknown, false); got != "stop_and_report" {
		t.Fatalf("unknown custody offered restart: %s", got)
	}
}

func TestSupervisorRuntimeRestartCommandsNeverInitializeMissingRoot(t *testing.T) {
	for _, action := range []string{"restart", "preflight"} {
		t.Run(action, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "missing")
			args := []string{"supervisor", "status", "--runtime-restart", "--root", root, "--json"}
			if action == "restart" {
				token := "v1:" + strings.Repeat("a", 32) + ":1:" + strings.Repeat("b", 32) + ":" + strings.Repeat("c", 64)
				args = []string{"supervisor", "restart", "--from-startup", token, "--root", root, "--json"}
			}
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr, Dependencies{})
			var response Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("invalid command response: code%d stderr%q: %v", code, stderr.String(), err)
			}
			if code != 1 || response.SupervisorControl == nil || response.SupervisorControl.State == "restarted" {
				t.Fatalf("missing root became a completed restart: code%d %+v", code, response)
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restart observation initialized state: %v", err)
			}
			if strings.Contains(response.Error, root) || strings.Contains(stderr.String(), root) {
				t.Fatal("restart command disclosed its private root")
			}
		})
	}
}

func TestSupervisorRuntimeRestartProjectionPreservesFaults(t *testing.T) {
	from := infrastructure.RuntimeRestartReference{RootID: strings.Repeat("a", 32), Startup: infrastructure.StartupReference{
		Sequence: 1, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64),
	}}
	for _, joined := range []bool{false, true} {
		response := newSupervisorCommandResponse("restart")
		response.SupervisorControl.RuntimeRestart = &SupervisorRuntimeRestartResponse{}
		cause := supervisor.ErrRuntimeQuiescenceBusy
		if joined {
			cause = errors.Join(cause, errors.New("private storage fault"))
		}
		result := finishRuntimeRestart(response, from, infrastructure.RuntimeRestartReport{}, cause)
		if joined {
			if result.code != 1 || result.err == nil || result.response.SupervisorControl.NextAction != "stop_and_report" {
				t.Fatalf("mixed failure became safe retry: %+v", result)
			}
		} else if result.code != 3 || result.response.SupervisorControl.State != "blocked" || result.response.SupervisorControl.NextAction != "retry_later" {
			t.Fatalf("pure quiescence refusal lost bounded classification: %+v", result)
		}
	}
}
