package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestTaskMaintenanceDeferralPreservesIndependentFailures(t *testing.T) {
	refused := pueue.ErrPrivateSupervisorTransitionIncomplete
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "no failure"},
		{name: "completion refused", err: refused, want: true},
		{name: "submission completion refused", err: pueue.ErrPrivateSupervisorSubmissionIncomplete, want: true},
		{name: "independent binding fault with submission refusal", err: errors.Join(pueue.ErrPrivateSupervisorSubmissionIncomplete, pueue.ErrBinding)},
		{name: "wrapped submission refusal", err: fmt.Errorf("repair: %w", pueue.ErrPrivateSupervisorSubmissionIncomplete), want: true},
		{name: "mixed completion refusals", err: errors.Join(refused, pueue.ErrPrivateSupervisorSubmissionIncomplete), want: true},
		{name: "migration fault and ordinary refusal", err: errors.Join(refused, pueue.ErrPrivateSupervisorMigrationRequired)},
		{name: "migration fault and submission refusal", err: errors.Join(pueue.ErrPrivateSupervisorSubmissionIncomplete, pueue.ErrPrivateSupervisorMigrationRequired)},
		{name: "cleanup fault and submission refusal", err: errors.Join(pueue.ErrPrivateSupervisorSubmissionIncomplete, os.ErrPermission)},
		{name: "pending command and submission refusal", err: errors.Join(pueue.ErrPrivateSupervisorSubmissionIncomplete, pueue.ErrInFlight)},
		{name: "expired command and submission refusal", err: errors.Join(pueue.ErrPrivateSupervisorSubmissionIncomplete, context.DeadlineExceeded)},
		{name: "wrapped refusal", err: fmt.Errorf("repair: %w", refused), want: true},
		{name: "joined refusals", err: errors.Join(refused, refused), want: true},
		{name: "generic migration", err: pueue.ErrPrivateSupervisorMigrationRequired},
		{name: "generic binding", err: pueue.ErrBinding},
		{name: "binding fault and refusal", err: errors.Join(refused, pueue.ErrBinding)},
		{name: "cleanup fault and refusal", err: errors.Join(refused, os.ErrPermission)},
		{name: "pending command and refusal", err: errors.Join(refused, pueue.ErrInFlight)},
		{name: "expired command and refusal", err: errors.Join(refused, context.DeadlineExceeded)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := TaskMaintenanceCompletionRefused(test.err); got != test.want {
				t.Errorf("maintenance deferral=%t, want %t for %v", got, test.want, test.err)
			}
		})
	}
}

func TestTransitionIncompleteKeepsMigrationAndBindingErrorContracts(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		binding   bool
		migration bool
		message   string
	}{
		{name: "ordinary completion", err: pueue.ErrPrivateSupervisorTransitionIncomplete, migration: true, message: "private supervisor migration is required: published private supervisor transition is incomplete"},
		{name: "submission gate", err: pueue.ErrPrivateSupervisorSubmissionIncomplete, binding: true, message: "pueue binding changed or unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := fmt.Errorf("completion check: %w", test.err)
			if migration := errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired); migration != test.migration {
				t.Errorf("migration classification=%t, want %t for %v", migration, test.migration, err)
			}
			if !TaskMaintenanceCompletionRefused(err) || test.err.Error() != test.message {
				t.Errorf("completion refusal changed its pre-mutation or message contract: %v", err)
			}
			if binding := errors.Is(err, pueue.ErrBinding); binding != test.binding {
				t.Errorf("binding classification=%t, want %t for %v", binding, test.binding, err)
			}
		})
	}
}
