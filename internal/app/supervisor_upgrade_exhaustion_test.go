package app

import (
	"strings"
	"testing"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSupervisorUpgradeExhaustedStartCannotBecomeRetryGuidance(t *testing.T) {
	binding := task.SupervisorRef{ClientExecutable: "/private/pueue", DaemonExecutable: "/private/pueued", ConfigPath: "/private/state/.supervisor/pueue.yml", ConfigDigest: strings.Repeat("b", 64), ResolvedConfigSHA256: strings.Repeat("c", 64), Endpoint: "unix:/private/socket", ObservedVersion: "pueue 4.0.4", ExecutableIdentity: task.ExecutableIdentityInstalledPathV1}
	for _, reason := range []string{"predecessor_birth_unknown", "predecessor_boot_changed", "predecessor_boot_unknown", "predecessor_wait_closed", "predecessor_not_ready", "predecessor_pid_mismatch", "predecessor_pair_missing", "successor_birth_unknown", "successor_boot_changed", "successor_wait_closed", "successor_pid_mismatch", "successor_not_ready"} {
		t.Run(reason, func(t *testing.T) {
			outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradePending, ReasonCode: reason, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, Report: infrastructure.UpgradeReport{OperationID: infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32)), Mode: infrastructure.UpgradeReactive, Phase: infrastructure.UpgradeDraining, Predecessor: binding}}
			result := finishSupervisorUpgrade(Arguments{}, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
			if result.err != nil || result.code != 3 || result.response.SupervisorControl.NextAction != "stop_and_report" {
				t.Fatalf("exhausted start proposed repeat: %+v", result)
			}
			diagnostic := newSupervisorCommandResponse("status")
			diagnostic.SupervisorControl.ObservedAt = "2026-11-20T18:00:00Z"
			diagnostic.SupervisorControl.MigrationReadiness = "blocked"
			diagnostic.SupervisorControl.FirstBlocker = &supervisorcore.MaintenanceBlocker{Kind: "task", ReasonCode: "runtime_closure_unproven", NextAction: "human_review_task"}
			result = finishSupervisorUpgradeDiagnostic(result, commandResult{response: diagnostic})
			control := result.response.SupervisorControl
			if control.ReasonCode != reason || control.NextAction != "stop_and_report" || control.FirstBlocker == nil {
				t.Fatalf("later diagnostic erased exhausted operation: %+v", control)
			}
		})
	}
}
