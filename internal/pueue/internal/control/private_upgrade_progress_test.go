package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestDispatchedPredecessorProgressRequiresExactDurableSubject(t *testing.T) {
	f, pending := upgradeUnclosedMigrationFixture(t, true)
	binding := f.client.Binding()
	for _, test := range []struct {
		name     string
		cause    error
		progress bool
	}{
		{"exact endpoint unavailable", ErrUnknown, true},
		{"unrelated missing file", os.ErrNotExist, false},
		{"cancelled observation", errors.Join(ErrUnknown, context.Canceled), false},
		{"joined storage fault", errors.Join(ErrUnknown, os.ErrPermission), false},
		{"joined identity fault", errors.Join(ErrUnknown, ErrBinding), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := dispatchedPredecessorObservationError(f.base, binding, pending, test.cause)
			var result *PrivateUpgradeError
			progress := errors.As(err, &result) && result.ReasonCode == "handoff_in_progress" && errors.Is(err, ErrPrivateUpgradePending)
			if progress != test.progress || !errors.Is(err, test.cause) {
				t.Fatalf("progress=%t err=%v", progress, err)
			}
		})
	}
	changed := pending
	changed.LegacyProcessStart = "different-incarnation"
	if err := dispatchedPredecessorObservationError(f.base, binding, changed, ErrUnknown); errors.Is(err, ErrPrivateUpgradePending) {
		t.Fatalf("changed journal subject became progress: %v", err)
	}
	requested := pending
	requested.LegacyShutdownDispatched = false
	if err := dispatchedPredecessorObservationError(f.base, binding, requested, ErrUnknown); errors.Is(err, ErrPrivateUpgradePending) {
		t.Fatalf("requested-only shutdown became progress: %v", err)
	}
	other := binding
	other.Endpoint = "unix:/unrelated-endpoint"
	if err := dispatchedPredecessorObservationError(f.base, other, pending, ErrUnknown); errors.Is(err, ErrPrivateUpgradePending) {
		t.Fatalf("unrelated endpoint became progress: %v", err)
	}
}

func TestMissingUpgradeExecutableNamesRoleAndPath(t *testing.T) {
	for _, role := range []string{"selected_client", "selected_daemon"} {
		t.Run(role, func(t *testing.T) {
			dir := canonicalTemp(t)
			client, daemon := filepath.Join(dir, "pueue"), filepath.Join(dir, "pueued")
			for _, path := range []string{client, daemon} {
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			missing := client
			if role == "selected_daemon" {
				missing = daemon
			}
			if err := os.Remove(missing); err != nil {
				t.Fatal(err)
			}
			err := validatePrivateUpgradePair(privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon})
			var fact *task.ControlFactError
			if !errors.As(err, &fact) || !errors.Is(err, os.ErrNotExist) || fact.ResourceID != role || fact.Name != missing || fact.ReasonCode != "installation_missing" {
				t.Fatalf("missing executable fact = %+v, err %v", fact, err)
			}
		})
	}
}

func TestExecutableFactJoinedAbsenceAndIOFaultIsUnusable(t *testing.T) {
	for _, absence := range []error{os.ErrNotExist, fmt.Errorf("%w: executable unavailable: %w", ErrConfiguration, os.ErrNotExist)} {
		cause := errors.Join(absence, unix.EIO)
		err := supervisorExecutableFact("recorded_client", "/fixture/missing/pueue", cause)
		var fact *task.ControlFactError
		if !errors.As(err, &fact) || fact.ReasonCode != "installation_unusable" || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, unix.EIO) {
			t.Fatalf("independent executable fault was classified as simple absence: %v", err)
		}
	}
}
