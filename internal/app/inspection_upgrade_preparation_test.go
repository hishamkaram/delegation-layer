package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// This composes the real terminal preparer and CLI projection. Native acceptance
// separately exercises the resident RPC and an actually observed/Waited worker.
func TestInspectionPreparationPreservesSettlementThroughUpgradeProjection(t *testing.T) {
	f := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	op := inspectionSettlementOperation(t, f, time.Now(), true)
	recordInspectionSettlementLifetime(t, op)
	pin, err := f.store.PinRootContext(t.Context())
	requireSettlementTest(t, err)
	var operation pueue.PrivateUpgradeObservation
	err = pueue.WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		var beginErr error
		operation, beginErr = pueue.BeginPrivateUpgradeWithLock(ctx, pin, pueue.UpgradeOperationRequired, f.options)
		if beginErr != nil {
			return beginErr
		}
		return preparePrivateTerminalEvidence(ctx, f.root, op.Request().Binding.Supervisor, nil)
	})
	var pending *supervisor.MaintenanceError
	if !supervisor.MaintenanceBusy(err) || !errors.As(err, &pending) || pending.Assessment.FirstBlocker == nil || pending.Assessment.FirstBlocker.ReasonCode != "inspection_closure_unproven" {
		t.Fatalf("terminal preparation replaced the supported settlement blocker: %v", err)
	}
	opID, parseErr := infrastructure.ParseUpgradeOperationID(string(operation.OperationID))
	requireSettlementTest(t, parseErr)
	outcome := infrastructure.UpgradeOutcome{
		Condition: infrastructure.UpgradePending,
		Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed},
		Report:    infrastructure.UpgradeReport{OperationID: opID, Mode: infrastructure.UpgradeOperationRequired, Phase: infrastructure.UpgradeDraining, Predecessor: operation.Predecessor},
	}
	result := finishSupervisorUpgrade(Arguments{}, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
	diagnostic := newSupervisorCommandResponse("status")
	diagnostic.SupervisorControl.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	applyMaintenanceAssessment(diagnostic.SupervisorControl, pending.Assessment)
	result = finishSupervisorUpgradeDiagnostic(result, commandResult{response: diagnostic})
	control := result.response.SupervisorControl
	if result.code != 3 || result.err != nil || control.State != "blocked" || control.NextAction != "settle_inspection" || control.Upgrade == nil || control.Upgrade.OperationID != string(operation.OperationID) {
		t.Fatalf("upgrade lost its settlement guidance or operation: %+v error=%v", control, result.err)
	}
}
