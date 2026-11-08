package app

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// Some callbacks have identical signatures; compilation cannot detect a swap
// between migration authorization and terminal evidence preparation.
func TestNativeInfrastructureCallbacksWireCanonicalFunctions(t *testing.T) {
	callbacks := nativeInfrastructureCallbacks()
	for name, same := range map[string]bool{
		"maintenance": reflect.ValueOf(callbacks.MaintenanceLaunch).Pointer() == reflect.ValueOf(supervisor.LaunchQueueMaintenanceOwnerWithEnvironment).Pointer(),
		"cleanup":     reflect.ValueOf(callbacks.SuccessfulCleanup).Pointer() == reflect.ValueOf(authorizePrivateSuccessfulCleanup).Pointer(),
		"evidence":    reflect.ValueOf(callbacks.TerminalEvidence).Pointer() == reflect.ValueOf(preparePrivateTerminalEvidence).Pointer(),
		"custody":     reflect.ValueOf(callbacks.TerminalCustody).Pointer() == reflect.ValueOf(releasePrivateTerminalCustody).Pointer(),
		"migration":   reflect.ValueOf(callbacks.MigrationAuthorization).Pointer() == reflect.ValueOf(authorizePrivateMigrationQueue).Pointer(),
	} {
		if !same {
			t.Errorf("native %s callback changed its lifecycle owner", name)
		}
	}
}

// The host owns wire decoding; the application owns the public recovery action
// for the decoded canonical causes and reason.
func TestHistoricalPreparationRefusalPublicGuidance(t *testing.T) {
	for _, independent := range []error{nil, errors.New("opaque host cleanup fault")} {
		cause := &pueue.PrivateRecoveryError{
			ReasonCode: "historical_target_not_ready",
			Err:        errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, infrastructure.ErrOwnerRequest, independent),
		}
		public := dispatchFailure(cause, "not_created")
		if public.Code != "supervisor_upgrade_required" || public.ReasonCode != "historical_target_not_ready" || public.NextAction != "stop_and_report" || public.RecoveryCommand != "" {
			t.Fatalf("historical refusal invited a migration loop: %+v", public)
		}
	}
}
