//go:build unix

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// TestQueueMaintenanceNeverRecreatesAbandonedRoot: a late terminal callback
// may run elected maintenance after acceptance disposal removed the state
// root, or against a root whose private supervisor was never published. Legacy
// adoption may only enter an existing bootstrap boundary, so these roots stay
// exactly as found and the maintenance attempt reports an error.
func TestQueueMaintenanceNeverRecreatesAbandonedRoot(t *testing.T) {
	for _, scenario := range []string{"deleted-root", "root-without-supervisor", "supervisor-without-bootstrap-lock"} {
		t.Run(scenario, func(t *testing.T) {
			root, base := abandonedRootScenario(t, scenario)
			before := existingRootEntries(t, root, base)

			err := RunQueueMaintenanceWithRuntime(root, AdmissionOptions{})
			if after := existingRootEntries(t, root, base); after != before {
				t.Errorf("abandoned root changed: before=%q after=%q", before, after)
			}
			if !errors.Is(err, pueue.ErrPrivateSupervisorNotInitialized) {
				t.Errorf("abandoned root maintenance error = %v, want %v", err, pueue.ErrPrivateSupervisorNotInitialized)
			}
		})
	}
}

// TestTerminalEventCallbackNeverRecreatesDeletedRoot: a late Pueue callback
// entering through RunTerminalEventWithRuntime after disposal deleted the state root
// records nothing, reports the refused request, keeps the root absent, and
// launches no supervisor executable from its lookup.
func TestTerminalEventCallbackNeverRecreatesDeletedRoot(t *testing.T) {
	root, base := abandonedRootScenario(t, "deleted-root")
	before := existingRootEntries(t, root, base)
	executables, lookup := launchMarkingLookup(t)

	err := RunTerminalEventWithRuntime(root, 1, "0", AdmissionOptions{InitialExecutable: lookup.InitialExecutable, Environment: lookup.Environment})
	if after := existingRootEntries(t, root, base); after != before {
		t.Errorf("deleted root changed: before=%q after=%q", before, after)
	}
	if !errors.Is(err, pueue.ErrConfiguration) {
		t.Errorf("deleted root callback error = %v, want %v", err, pueue.ErrConfiguration)
	}
	entries, readErr := os.ReadDir(executables)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != len(launchMarkingNames) {
		t.Errorf("callback launched a lookup executable: %d entries, want %d", len(entries), len(launchMarkingNames))
	}
}

// launchMarkingNames are the executables a lookup may select; each one leaves
// a sibling marker when launched.
var launchMarkingNames = []string{"delegate", "pueue", "pueued"}

// launchMarkingLookup returns a canonical directory of marking executables and
// a lookup that selects only them.
func launchMarkingLookup(t *testing.T) (string, installation.ExecutableLookup) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range launchMarkingNames {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n: > \"$0.launched\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return directory, installation.ExecutableLookup{
		InitialExecutable: filepath.Join(directory, "delegate"),
		Environment:       []string{"PATH=" + directory},
	}
}

// abandonedRootScenario returns a state root under a canonical existing parent,
// so only the intentionally absent entries can fail path validation.
func abandonedRootScenario(t *testing.T, scenario string) (root, base string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(parent, "state")
	base = filepath.Join(root, ".supervisor")
	switch scenario {
	case "root-without-supervisor":
		err = os.Mkdir(root, 0o700)
	case "supervisor-without-bootstrap-lock":
		err = os.MkdirAll(base, 0o700)
	}
	if err != nil {
		t.Fatal(err)
	}
	return root, base
}

// existingRootEntries summarizes which of the root, its private supervisor
// directory, and the bootstrap lock exist, plus the supervisor directory's
// entries, so any manufactured state changes the result.
func existingRootEntries(t *testing.T, root, base string) string {
	t.Helper()
	summary := ""
	for _, path := range []string{root, base, filepath.Join(base, "bootstrap.lock")} {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			summary += "present;"
		case errors.Is(err, os.ErrNotExist):
			summary += "absent;"
		default:
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(base)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		summary += entry.Name() + ","
	}
	return summary
}
