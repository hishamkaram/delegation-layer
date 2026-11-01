package tasklifecycle

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func teamContinuationReplayRequest(fixture teamDispatchFixture) DispatchRequest {
	return DispatchRequest{
		Root: fixture.store.Root, TaskID: strings.Repeat("c", 32), ResumeTask: fixture.request.TaskID,
		Provider: fixture.request.Provider, Cwd: fixture.request.CanonicalCwd, BriefBytes: bytes.Clone(fixture.brief),
		Config: task.TaskConfig{Permission: "read-only", Budget: "1m", Model: "successor-model-a", Effort: "successor-effort-a"},
	}
}

func teamContinuationReplayRecord(t *testing.T, fixture teamDispatchFixture, request DispatchRequest) task.TaskRecord {
	t.Helper()
	record, err := admissioncore.BuildRequest(fixture.store.RootID, request.TaskID, request.Provider, request.Cwd, request.Config, fixture.brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	record.Team = fixture.request.Team.Clone()
	record.PriorSession = &task.PriorSession{Provider: fixture.request.Provider, ConversationID: "recorded-conversation", PredecessorTaskID: fixture.request.TaskID}
	return record
}

func persistTeamContinuationReplay(t *testing.T, fixture teamDispatchFixture, record task.TaskRecord) *taskdir.TaskDir {
	t.Helper()
	meta := *fixture.meta
	meta.TaskID, meta.RequestedConfig = record.TaskID, record.RequestedConfig
	td, err := fixture.store.CreateTask(record.TaskID, &record, fixture.brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = td.ClaimSession(record.Provider, record.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	sealTeamPredecessor(t, teamDispatchFixture{store: fixture.store, td: td, request: &record, meta: &meta, brief: fixture.brief})
	return td
}

func dispatchTeamContinuationReplay(fixture teamDispatchFixture, request DispatchRequest, options DispatchOptions) DispatchResult {
	result := DispatchResult{Root: fixture.store.Root, RootID: fixture.store.RootID, TaskID: request.TaskID}
	return dispatchPrepared(request, options, fixture.store, request.Cwd, fixture.brief, nil, result)
}

func TestTeamContinuationReplayExistingRecordKeepsSuccessorOverrides(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	sealTeamPredecessor(t, fixture)
	request := teamContinuationReplayRequest(fixture)
	winner := teamContinuationReplayRecord(t, fixture, request)
	td := persistTeamContinuationReplay(t, fixture, winner)
	before, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	options := teamResumeOptions(t, fixture)
	for _, field := range []string{"matching", "model", "effort"} {
		t.Run(field, func(t *testing.T) {
			candidate := request
			switch field {
			case "model":
				candidate.Config.Model = "successor-model-b"
			case "effort":
				candidate.Config.Effort = "successor-effort-b"
			}
			result := dispatchTeamContinuationReplay(fixture, candidate, options)
			requireTeamContinuationReplayResult(t, result, field == "matching")
		})
	}
	after, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("replaying successor rewrote its immutable request: %v", err)
	}
	saved, err := td.RecordedRequest()
	if err != nil || !reflect.DeepEqual(saved, &winner) {
		t.Fatalf("replay erased successor choices or inherited Team context: %+v, %v", saved, err)
	}
}

func requireTeamContinuationReplayResult(t *testing.T, result DispatchResult, matching bool) {
	t.Helper()
	if matching {
		if err := result.Err(); err != nil {
			t.Fatalf("matching successor replay was rejected: %v; existing=%+v", err, result.Existing)
		}
		if result.Existing == nil || result.Creation != nil {
			t.Fatalf("matching replay did not use the existing-record owner: %+v", result)
		}
		return
	}
	if !errors.Is(result.Err(), task.ErrRequestConflict) {
		t.Fatalf("changed successor choices were erased by inherited Team.Intent: %v; want ErrRequestConflict", result.Err())
	}
}

func TestTeamContinuationReplayCreationConflictKeepsSuccessorOverrides(t *testing.T) {
	for _, field := range []string{"model", "effort"} {
		t.Run(field, func(t *testing.T) {
			fixture := newTeamDispatchFixture(t)
			sealTeamPredecessor(t, fixture)
			request := teamContinuationReplayRequest(fixture)
			winner := teamContinuationReplayRecord(t, fixture, request)
			if field == "model" {
				request.Config.Model = "successor-model-b"
			} else {
				request.Config.Effort = "successor-effort-b"
			}
			calls := 0
			options := teamContinuationCreationRaceOptions(t, fixture, &request, winner, &calls)
			result := dispatchTeamContinuationReplay(fixture, request, options)
			if calls != 2 || result.Creation == nil || result.Creation.Stage != CreationRecord {
				t.Fatalf("fixture did not reach the real record-creation conflict: calls=%d result=%+v error=%v", calls, result, result.Err())
			}
			requireTeamContinuationReplayResult(t, result, false)
			td, err := fixture.store.OpenTask(winner.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			saved, readErr := td.RecordedRequest()
			closeErr := td.Close()
			if err = errors.Join(readErr, closeErr); err != nil || !reflect.DeepEqual(saved, &winner) {
				t.Fatalf("creation conflict changed the recorded successor: %+v, %v", saved, err)
			}
		})
	}
}

func teamContinuationCreationRaceOptions(t *testing.T, fixture teamDispatchFixture, request *DispatchRequest, winner task.TaskRecord, calls *int) DispatchOptions {
	t.Helper()
	supervisor, configPath := schedulerDispatchSupervisor(t, t.TempDir())
	runner := filepath.Join(fixture.request.CanonicalCwd, "continuation-runner")
	supervisorfixture.WriteExecutable(t, runner, "#!/bin/sh\nexit 64\n")
	request.Selection.Runner, request.Selection.ConfigPath = runner, configPath
	prepare := commonprovider.ReadyCandidate(func(candidate task.TaskRecord) (commonprovider.PreparedProfile, error) {
		return commonprovider.PreparedProfile{
			Plan:            execution.Plan{Executable: runner, Directory: candidate.CanonicalCwd, Environment: []string{}, Predicate: task.FixturePredicateRef()},
			Effective:       fixture.meta.EffectiveConfig,
			ObservedVersion: fixture.meta.ProviderVersion,
		}, nil
	})
	source := teamResumeCandidates{prepare: func(candidate task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
		*calls += 1
		// The first call validates the predecessor continuation. The second
		// runs only after dispatchPrepared observed the successor as absent.
		if *calls == 2 {
			persistTeamContinuationReplay(t, fixture, winner)
		}
		return prepare(candidate, environment)
	}}
	profiles := admissioncore.NewProfiles(source, nil, []string{})
	options := teamResumeOptions(t, fixture)
	options.Creation.PublisherVersion = "continuation-replay-test"
	options.Creation.Admission.Profiles = profiles
	options.Creation.Admission.Supervisor.InitialExecutable = supervisor
	options.Creation.Admission.DescribeProvider = func(id string) (commonprovider.Description, error) {
		registration, err := options.Catalog.Lookup(id)
		return registration.Description, err
	}
	options.ContinuationProfiles = func(task.MetaRecord, []string) admissioncore.Profiles { return profiles }
	return options
}
