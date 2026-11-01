package taskdir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func recordedTeamTask(t *testing.T) (*TaskDir, *task.TaskRecord) {
	t.Helper()
	store := testStore(t)
	request, meta, brief := preparedInput(t, store)
	request.Team = &task.TeamContext{
		SchemaVersion: 1, TeamRevision: 3, TeamName: "Review team", TeamPurpose: "Review changes.", MemberName: "Primary",
		Target: task.TeamTarget{
			Team:     task.TeamReference{Scope: "project", TeamID: "review-team", Project: &task.TeamProject{Kind: "directory", Path: request.CanonicalCwd}},
			MemberID: "primary", CatalogID: "0123456789abcdef0123456789abcdef",
		},
		Roles:  []task.TeamRoleContext{{ID: "reviewer", Name: "Reviewer", Responsibility: "Check correctness."}},
		Intent: task.TeamIntent{Config: task.TaskConfig{ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"}}},
	}
	td, err := store.CreateTask(request.TaskID, request, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	return td, request
}

func TestRecordedRequestReadsImmutableFragmentWithoutMeta(t *testing.T) {
	td, want := recordedTeamTask(t)
	path := filepath.Join(td.Dir, "task.json")
	before := readTestFile(t, path)
	must(t, os.Remove(filepath.Join(td.Dir, "meta.json")))
	got, err := td.RecordedRequest()
	must(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded fragment = %+v, want %+v", got, want)
	}
	got.Team.Target.Team.Project.Path = filepath.Join(want.CanonicalCwd, "other")
	got.Team.Roles[0].Responsibility = "Changed caller value."
	got.Team.Intent.Config.ReviewTarget.Value = "other-branch"
	again, err := td.RecordedRequest()
	must(t, err)
	if !reflect.DeepEqual(again, want) || !bytes.Equal(readTestFile(t, path), before) {
		t.Fatal("caller mutation changed recorded immutable request")
	}
	testAbsent(t, filepath.Join(td.Dir, "meta.json"))
	requireRecordedReadCannotLaunch(t, td)
}

func requireRecordedReadCannotLaunch(t *testing.T, td *TaskDir) {
	t.Helper()
	if _, _, err := td.PreparedRecords(); err == nil {
		t.Error("fragment read granted complete prepared-set validity")
	}
	start, startErr := td.PrepareStart(0)
	if start != nil {
		must(t, start.Release())
		t.Error("fragment read granted a start permit")
	}
	if startErr == nil {
		t.Error("incomplete prepared set accepted by PrepareStart")
	}
	submit, submitErr := td.PrepareSubmission(task.SupervisorRef{})
	if submit != nil {
		must(t, submit.Release())
		t.Error("fragment read granted a submission permit")
	}
	if submitErr == nil {
		t.Error("incomplete prepared set accepted by PrepareSubmission")
	}
	for _, name := range []string{"submit.json", "provider.start"} {
		testAbsent(t, filepath.Join(td.Dir, name))
	}
}

func TestRecordedRequestDoesNotValidateOtherPreparedFragments(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testing.T, *TaskDir)
	}{
		{"missing brief", func(t *testing.T, td *TaskDir) { must(t, os.Remove(filepath.Join(td.Dir, "brief.md"))) }},
		{"changed brief", func(t *testing.T, td *TaskDir) {
			writeTestFile(t, filepath.Join(td.Dir, "brief.md"), []byte("changed"))
		}},
		{"invalid metadata", func(t *testing.T, td *TaskDir) { writeTestFile(t, filepath.Join(td.Dir, "meta.json"), []byte(`{}`)) }},
		{"invalid submission guard", func(t *testing.T, td *TaskDir) { writeTestFile(t, filepath.Join(td.Dir, "submit.json"), []byte(`{}`)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td, want := recordedTeamTask(t)
			tc.edit(t, td)
			got, err := td.RecordedRequest()
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("unrelated fragment prevented request read: %+v, %v", got, err)
			}
			if _, _, err := td.PreparedRecords(); err == nil {
				t.Fatal("request-only validity granted prepared-set authority")
			}
		})
	}
}

func TestRecordedRequestRejectsInvalidIdentityAndTeam(t *testing.T) {
	cases := []struct {
		name string
		edit func(*task.TaskRecord)
		want error
	}{
		{"root mismatch", func(r *task.TaskRecord) { r.RootID = strings.Repeat("a", 32) }, task.ErrIdentityMismatch},
		{"task mismatch", func(r *task.TaskRecord) { r.TaskID = strings.Repeat("a", 32) }, task.ErrIdentityMismatch},
		{"unsupported schema", func(r *task.TaskRecord) { r.SchemaVersion++ }, task.ErrUnsupportedSchema},
		{"invalid Team revision", func(r *task.TaskRecord) { r.Team.TeamRevision = 0 }, task.ErrInvalidTeamSelection},
		{"invalid Team catalog", func(r *task.TaskRecord) { r.Team.Target.CatalogID = "" }, task.ErrInvalidTeamSelection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			td, request := recordedTeamTask(t)
			tc.edit(request)
			data, err := task.MarshalCanonical(request)
			must(t, err)
			writeTestFile(t, filepath.Join(td.Dir, "task.json"), data)
			got, err := td.RecordedRequest()
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("RecordedRequest = %+v, %v; want no record and %v", got, err, tc.want)
			}
		})
	}
}

func TestRecordedRequestRetainsStrictReadBoundary(t *testing.T) {
	for _, data := range [][]byte{[]byte(`{}`), []byte(`{"schema_version":1,"schema_version":1}`), []byte(`null`), bytes.Repeat([]byte(" "), task.MaxControlRecordSize+1)} {
		td, _ := recordedTeamTask(t)
		writeTestFile(t, filepath.Join(td.Dir, "task.json"), data)
		if record, err := td.RecordedRequest(); err == nil || record != nil {
			t.Errorf("invalid request bytes accepted: %+v, %v", record, err)
		}
	}
	t.Run("missing request", func(t *testing.T) {
		td, _ := recordedTeamTask(t)
		must(t, os.Remove(filepath.Join(td.Dir, "task.json")))
		if record, err := td.RecordedRequest(); record != nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing request = %+v, %v; want absence", record, err)
		}
	})
	t.Run("unsafe request permissions", func(t *testing.T) {
		td, _ := recordedTeamTask(t)
		must(t, os.Chmod(filepath.Join(td.Dir, "task.json"), 0o644))
		if record, err := td.RecordedRequest(); record != nil || !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("unsafe request = %+v, %v; want evidence fault", record, err)
		}
	})
}
