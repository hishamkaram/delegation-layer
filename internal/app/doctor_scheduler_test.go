package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestDoctorSchedulerExistingEmptyDatabaseIsAbsent(t *testing.T) {
	state := doctorSchedulerState(t)
	path := filepath.Join(state.Root, ".scheduler", "schedules.sqlite")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertDoctorSchedulerObservation(t, state.Root, scheduler.CompatibilityAbsent)
}

func TestDoctorSchedulerReadOnlyFailureAdvisories(t *testing.T) {
	for _, tc := range []struct {
		name string
		want scheduler.Compatibility
	}{
		{"invalid_magic", scheduler.CompatibilityIncompatible},
		{"legacy_v1", scheduler.CompatibilityMigrationRequired},
		{"newer_schema", scheduler.CompatibilityNewerSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDoctorSchedulerObservation(t, doctorSchedulerFixture(t, tc.name), tc.want)
		})
	}
}

func doctorSchedulerFixture(t *testing.T, name string) string {
	t.Helper()
	if name == "legacy_v1" {
		return materializeLegacySchedule(t)
	}
	state := doctorSchedulerState(t)
	path := filepath.Join(state.Root, ".scheduler", "schedules.sqlite")
	if name == "invalid_magic" {
		if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 24), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		owner, err := scheduler.AcquireOwnerLease(t.Context(), state)
		if err != nil {
			t.Fatal(err)
		}
		migrateErr := owner.Migrate(t.Context())
		closeErr := owner.Close()
		if migrateErr != nil || closeErr != nil {
			t.Fatalf("fixture initialize: migrate=%v close=%v", migrateErr, closeErr)
		}
		doctorSchedulerNewerVersion(t, path)
	}
	return state.Root
}

func doctorSchedulerState(t *testing.T) *taskdir.Store {
	t.Helper()
	state, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	return state
}

func assertDoctorSchedulerObservation(t *testing.T, root string, want scheduler.Compatibility) {
	t.Helper()
	directory := filepath.Join(root, ".scheduler")
	// Empty-database observation must create no sidecar. The failure cases
	// separately preserve an existing non-hot journal byte for byte.
	if want != scheduler.CompatibilityAbsent {
		if err := os.WriteFile(filepath.Join(directory, "schedules.sqlite-journal"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := doctorSchedulerFiles(t, directory)
	report, code := observeDoctor(Arguments{Root: root}, Dependencies{}, doctorBundle(t), nil)
	if code != 0 || len(report.Issues) != 0 || report.Scheduler.Compatibility != want || report.Scheduler.Verification != "read_only" {
		t.Fatalf("advisory changed doctor exit policy: %+v code=%d", report, code)
	}
	issueCount := 0
	if want != scheduler.CompatibilityAbsent {
		issueCount = 1
	}
	if len(report.Scheduler.Issues) != issueCount {
		t.Fatalf("advisory issues: %+v", report.Scheduler)
	}
	assertPublicDoctorScheduler(t, root, want, issueCount)
	if after := doctorSchedulerFiles(t, directory); !reflect.DeepEqual(after, before) {
		t.Fatalf("doctor changed scheduler bytes/entries/sidecars: before=%v after=%v", before, after)
	}
}

func assertPublicDoctorScheduler(t *testing.T, root string, want scheduler.Compatibility, issueCount int) {
	t.Helper()
	for _, jsonOutput := range []bool{true, false} {
		args := []string{"doctor", "--root", root}
		if jsonOutput {
			args = append(args, "--json")
		}
		var output, stderr bytes.Buffer
		// The test executable is not an installation bundle: preserve exit 2 for
		// missing roles while the scheduler remains a separate passive advisory.
		code := Run(args, &output, &stderr, Dependencies{Environment: []string{}})
		if code != 2 {
			t.Fatalf("public doctor exit changed: code=%d stderr=%s", code, stderr.String())
		}
		if jsonOutput {
			var public DoctorReport
			if err := json.Unmarshal(output.Bytes(), &public); err != nil {
				t.Fatal(err)
			}
			if public.Scheduler.Compatibility != want || public.Scheduler.Verification != "read_only" || len(public.Scheduler.Issues) != issueCount {
				t.Fatalf("public scheduler advisory: %+v", public.Scheduler)
			}
		} else if !strings.Contains(output.String(), "scheduler compatibility="+string(want)) {
			t.Fatalf("human advisory missing: %s", output.String())
		}
	}
}

func doctorSchedulerFiles(t *testing.T, directory string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = data
	}
	return files
}

func doctorSchedulerNewerVersion(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version=3"); err != nil {
		t.Fatal(err)
	}
}
