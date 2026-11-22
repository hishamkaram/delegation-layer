package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestColdUpgradeDriverContinuesBeyondFourCleanPending(t *testing.T) {
	const root = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const operation = "upgrade-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ticks := make(chan time.Time, 6)
	for range 6 {
		ticks <- time.Time{}
	}
	calls := 0
	ctx := t.Context()
	result, err := driveColdUpgrade(ctx, root, "", infrastructure.UpgradeMigrating, func(callContext context.Context, attempt int) coldUpgradeCommandResult {
		if callContext != ctx {
			t.Fatal("driver replaced the caller context")
		}
		calls++
		command := coldUpgradeDriverPending(root, operation, "predecessor_wait_pending")
		command.diagnostic.Attempt = attempt
		if calls == 7 {
			command.code = 0
			command.response.SupervisorControl.Upgrade.Completed = true
		}
		return command
	}, ticks)
	if err != nil {
		t.Fatalf("six clean pending responses must reach completion: stopped after %d calls: %v", calls, err)
	}
	if calls != 7 || result.totalAttempts != 7 || !result.view.Completed || result.operation != operation {
		t.Fatalf("exact operation did not complete after six pending responses: calls=%d attempts=%d completed=%t", calls, result.totalAttempts, result.view.Completed)
	}
	if len(result.attempts) != 4 {
		t.Fatalf("diagnostic history grew beyond its four retained records: %d", len(result.attempts))
	}
	for index, attempt := range result.attempts {
		if attempt.Attempt != index+4 {
			t.Fatalf("history did not retain the last four attempts: index=%d attempt=%d", index, attempt.Attempt)
		}
	}
}

func TestColdUpgradeDriverReconcilesCustodyPending(t *testing.T) {
	const root = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const operation = "upgrade-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	calls := 0
	result, err := driveColdUpgrade(t.Context(), root, operation, infrastructure.UpgradeMigrating, func(context.Context, int) coldUpgradeCommandResult {
		calls++
		command := coldUpgradeDriverPending(root, operation, "predecessor_custody_unavailable")
		if calls == 2 {
			command.code = 0
			command.response.SupervisorControl.Upgrade.Completed = true
		}
		return command
	}, ticks)
	if err != nil || calls != 2 || !result.view.Completed {
		t.Fatalf("clean custody pending did not reconcile on the same A: calls=%d completed=%t err=%v", calls, result.view.Completed, err)
	}
}

func TestColdUpgradeDriverStopsWithoutAnotherCall(t *testing.T) {
	const root = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const operation = "upgrade-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const private = "PRIVATE-COLD-DRIVER-FAULT"
	for _, test := range []struct {
		name   string
		change func(*coldUpgradeCommandResult)
	}{
		{name: "hard command fault", change: func(command *coldUpgradeCommandResult) { command.err = errors.New(private) }},
		{name: "mixed command fault", change: func(command *coldUpgradeCommandResult) {
			command.err = errors.Join(context.DeadlineExceeded, errors.New(private))
		}},
		{name: "public error", change: func(command *coldUpgradeCommandResult) { command.response.Error = private }},
		{name: "truncated error", change: func(command *coldUpgradeCommandResult) { command.response.ErrorTruncated = true }},
		{name: "typed hard fact", change: func(command *coldUpgradeCommandResult) {
			command.response.ControlFact = &ControlFactResponse{ReasonCode: "evidence_unreadable"}
		}},
		{name: "wrong root", change: func(command *coldUpgradeCommandResult) { command.response.RootID = "cccccccccccccccccccccccccccccccc" }},
		{name: "changed A", change: func(command *coldUpgradeCommandResult) {
			command.response.SupervisorControl.Upgrade.OperationID = "upgrade-v1:cccccccccccccccccccccccccccccccc"
		}},
		{name: "malformed A", change: func(command *coldUpgradeCommandResult) {
			command.response.SupervisorControl.Upgrade.OperationID = private
		}},
		{name: "wrong schema", change: func(command *coldUpgradeCommandResult) { command.response.SchemaVersion = OutputSchemaVersion + 1 }},
		{name: "wrong command", change: func(command *coldUpgradeCommandResult) { command.response.Command = "status" }},
		{name: "missing control", change: func(command *coldUpgradeCommandResult) { command.response.SupervisorControl = nil }},
		{name: "missing upgrade", change: func(command *coldUpgradeCommandResult) { command.response.SupervisorControl.Upgrade = nil }},
		{name: "wrong exit", change: func(command *coldUpgradeCommandResult) { command.code = 1 }},
		{name: "wrong action", change: func(command *coldUpgradeCommandResult) {
			command.response.SupervisorControl.NextAction = "run_supervisor_migrate"
		}},
		{name: "unknown birth reason", change: func(command *coldUpgradeCommandResult) {
			command.response.SupervisorControl.ReasonCode = "predecessor_birth_unknown"
		}},
		{name: "wrong success phase", change: func(command *coldUpgradeCommandResult) {
			command.code = 0
			command.response.SupervisorControl.Upgrade.Phase = "awaiting_installation"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ticks := make(chan time.Time, 2)
			ticks <- time.Time{}
			ticks <- time.Time{}
			calls := 0
			result, err := driveColdUpgrade(t.Context(), root, operation, infrastructure.UpgradeMigrating, func(context.Context, int) coldUpgradeCommandResult {
				calls++
				if calls > 2 {
					return coldUpgradeCommandResult{}
				}
				command := coldUpgradeDriverPending(root, operation, "predecessor_wait_pending")
				if calls == 2 {
					test.change(&command)
				}
				return command
			}, ticks)
			if err == nil || calls != 2 || result.view.Completed {
				t.Fatalf("fault permitted another call or completion: calls=%d completed=%t err=%v", calls, result.view.Completed, err)
			}
		})
	}
}

func TestColdUpgradeDriverCanceledBeforeFirstCall(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	result, err := driveColdUpgrade(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", infrastructure.UpgradeMigrating, func(context.Context, int) coldUpgradeCommandResult {
		calls++
		return coldUpgradeCommandResult{}
	}, nil)
	if !errors.Is(err, context.Canceled) || calls != 0 || result.view.Completed {
		t.Fatalf("already canceled context permitted work: calls=%d completed=%t err=%v", calls, result.view.Completed, err)
	}
}

func TestColdUpgradeDriverCancellationDuringPendingJoins(t *testing.T) {
	const root = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const operation = "upgrade-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ctx, cancel := context.WithCancel(t.Context())
	firstCall, joined := make(chan struct{}), make(chan struct{})
	calls := 0
	var result coldUpgradeDriverResult
	var err error
	t.Cleanup(func() {
		cancel()
		<-joined
	})
	go func() {
		defer close(joined)
		result, err = driveColdUpgrade(ctx, root, operation, infrastructure.UpgradeMigrating, func(context.Context, int) coldUpgradeCommandResult {
			calls++
			if calls == 1 {
				close(firstCall)
			}
			return coldUpgradeDriverPending(root, operation, "predecessor_wait_pending")
		}, nil)
	}()
	select {
	case <-firstCall:
	case <-joined:
		t.Fatal("driver stopped before the controlled first pending response")
	}
	cancel()
	<-joined
	if !errors.Is(err, context.Canceled) || calls != 1 || result.view.Completed {
		t.Fatalf("canceled pending issued another request or completed: calls=%d completed=%t err=%v", calls, result.view.Completed, err)
	}
}

func TestColdUpgradeDriverCanceledCompletionIsNotSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	result, err := driveColdUpgrade(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", infrastructure.UpgradeMigrating, func(context.Context, int) coldUpgradeCommandResult {
		calls++
		command := coldUpgradeDriverPending("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "upgrade-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "predecessor_wait_pending")
		command.code = 0
		command.response.SupervisorControl.Upgrade.Completed = true
		cancel()
		return command
	}, nil)
	if !errors.Is(err, context.Canceled) || calls != 1 || result.view.Completed {
		t.Fatalf("completion erased caller cancellation: calls=%d completed=%t err=%v", calls, result.view.Completed, err)
	}
}

func TestColdUpgradeDriverExpiredContextNeverCompletes(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()
	calls := 0
	result, err := driveColdUpgrade(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", infrastructure.UpgradeMigrating, func(context.Context, int) coldUpgradeCommandResult {
		calls++
		return coldUpgradeCommandResult{}
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 0 || result.view.Completed {
		t.Fatalf("expired workflow invented completion: calls=%d completed=%t err=%v", calls, result.view.Completed, err)
	}
}

func TestColdUpgradeDriverFailureHistoryExcludesPrivateInputs(t *testing.T) {
	const private = "PRIVATE-COLD-DRIVER-ERROR-TOKEN-ARGV"
	fixture := &runtimeRestartCLINative{root: t.TempDir(), rootID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	result, err := driveColdUpgrade(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", infrastructure.UpgradeMigrating, func(context.Context, int) coldUpgradeCommandResult {
		command := coldUpgradeDriverPending("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "upgrade-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "predecessor_wait_pending")
		command.err = errors.New(private)
		command.response.Error = private
		command.response.SupervisorControl.Upgrade.OperationID = private
		command.response.SupervisorControl.Upgrade.CompletionCommand = []string{private}
		command.response.SupervisorControl.RuntimeRestart = &SupervisorRuntimeRestartResponse{FromStartup: private, SuccessorStartup: private}
		command.diagnostic = coldUpgradeAttemptFacts(fixture, command.response, 1, command.code, command.err)
		return command
	}, nil)
	if err == nil || strings.Contains(err.Error(), private) {
		t.Fatal("driver returned an unsafe error or accepted the private-error response")
	}
	if len(result.attempts) != 1 || result.attempts[0].Attempt != 1 || !result.attempts[0].CommandError || !result.attempts[0].ResponseError || !result.attempts[0].RootMatches || !result.attempts[0].UpgradePresent {
		t.Fatal("privacy check did not retain the actual populated per-attempt projection")
	}
	facts := coldUpgradeFailureFacts(result.lastResponse, []string{"ensure-owner", "--complete-upgrade", private}, "", false, errors.New(private))
	facts.Attempts, facts.TotalAttempts = result.attempts, result.totalAttempts
	data, encodeErr := json.Marshal(facts)
	if encodeErr != nil {
		t.Fatal("driver failure history could not be encoded")
	}
	if strings.Contains(string(data), private) {
		t.Fatal("serialized failure history exposed the private canary")
	}
}

func coldUpgradeDriverPending(root, operation, reason string) coldUpgradeCommandResult {
	return coldUpgradeCommandResult{
		response: Response{
			SchemaVersion: OutputSchemaVersion, Command: "supervisor", RootID: root,
			SupervisorControl: &SupervisorControlResponse{
				State: "blocked", MigrationPhase: "migrating", ReasonCode: reason, NextAction: "repeat_supervisor_ensure_owner",
				Upgrade: &SupervisorUpgradeResponse{OperationID: operation, Phase: "migrating"},
			},
		},
		code: 3,
	}
}
