package infrastructurehost

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestReadyCurrentUncompletedOperationRemainsTypedRefusal(t *testing.T) {
	// The control fence withholds a completed-A projection when A is still
	// pending. The resident must transport that refusal without a false report.
	outcome, err := upgradeOutcome(pueue.PrivateUpgradeObservation{}, pueue.ErrPrivateUpgradeOperation)
	if err != nil || outcome.Condition != infrastructure.UpgradeRefused || outcome.Report != (infrastructure.UpgradeReport{}) || outcome.ReasonCode != "upgrade_operation_mismatch" {
		t.Fatalf("uncompleted A became invalid host evidence: %+v %v", outcome, err)
	}
}
