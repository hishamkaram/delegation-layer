package taskdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdmissionOnlyObservationIsReadOnlyAndRequiresNoReceipt(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	supervisorSubmit(t, td)
	before, err := os.ReadDir(td.Dir)
	must(t, err)
	eligible, err := td.ReadAdmissionOnlyContext(t.Context())
	must(t, err)
	if !eligible {
		t.Fatal("submitted task without execution evidence did not offer explicit cancellation")
	}
	after, err := os.ReadDir(td.Dir)
	must(t, err)
	if len(before) != len(after) {
		t.Fatal("read-only observation created task files")
	}
	for _, name := range []string{"lifetime", "outcome.json", "operator-disposition.json"} {
		if _, err = os.Stat(filepath.Join(td.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("observation created %s: %v", name, err)
		}
	}
	supervisorReceipt(t, td)
	eligible, err = td.ReadAdmissionOnlyContext(t.Context())
	must(t, err)
	if eligible {
		t.Fatal("receipted task was classified admission-only")
	}
}

func TestAdmissionOnlyObservationRejectsExecutionAndUnreadableSubmission(t *testing.T) {
	for _, name := range []string{"provider.start", "submit.json"} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			td := supervisorTask(t, store)
			supervisorSubmit(t, td)
			must(t, os.WriteFile(filepath.Join(td.Dir, name), []byte("invalid"), 0o600))
			eligible, err := td.ReadAdmissionOnlyContext(t.Context())
			if eligible {
				t.Fatal("invalid or execution evidence offered admission-only guidance")
			}
			if name == "submit.json" && err == nil {
				t.Fatal("unreadable submission was treated as absence")
			}
		})
	}
}
