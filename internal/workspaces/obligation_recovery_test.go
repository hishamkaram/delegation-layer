package workspaces

import (
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceReleaseRecoversObligationAfterReservationCrash(t *testing.T) {
	for _, discard := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain-on-capture-failure", true: "discard-authorized"}[discard], func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			action := workspace.ReleaseAction{
				SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID,
				ID: strings.Repeat("e", 32), Discard: discard, BudgetNanos: int64(37 * time.Second),
			}
			subject, err := fixture.Resource.RecordReleaseAction(t.Context(), action)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := fixture.Resource.Reserve(t.Context(), subject)
			if err != nil {
				t.Fatal(err)
			}
			fixture.Restart(t)
			workspacefixture.RecoveryUseOwner(t, fixture)
			if err = restoreWorkspaceCaptureObligation(t.Context(), fixture.Store, fixture.Resource, reservation); err != nil {
				t.Fatal(err)
			}
			obligation, err := fixture.Resource.CaptureObligation(t.Context(), reservation.Sequence)
			if err != nil || obligation.Subject != subject || obligation.Predecessor != nil || obligation.BudgetNanos != action.BudgetNanos {
				t.Fatalf("recovered release obligation = %+v, error = %v", obligation, err)
			}
			if err = fixture.Resource.ReleaseCaptureAuthorized(t.Context(), obligation); err != nil {
				t.Fatalf("recovered release lost original authorization: %v", err)
			}
		})
	}
}
