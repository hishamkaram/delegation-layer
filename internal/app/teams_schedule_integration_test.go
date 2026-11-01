package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	schedulecontract "github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams"
)

func TestTeamsScheduleCreatePinsCatalogAndPreservesRawOmissions(t *testing.T) {
	for _, options := range []task.TaskConfig{{}, {Model: "caller-model", Effort: "medium", Permission: "read-only", Budget: "2m"}} {
		t.Run("model="+options.Model, func(t *testing.T) {
			f := newTeamsScheduleFixture(t)
			f.arguments.Config = options
			before, readErr := os.ReadFile(f.deps.TeamCatalogPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			definition := f.create(t)
			request := definition.Request
			teamsScheduleAssertPinnedRequest(t, request, f.catalogID, options)
			if len(f.captured()) != 0 {
				t.Fatal("schedule creation prepared a provider")
			}
			teamsScheduleAssertNoTaskEntries(t, f.state.Root)
			after, readErr := os.ReadFile(f.deps.TeamCatalogPath)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("schedule creation mutated catalog/defaults: %v", readErr)
			}
			stored, _ := teamsScheduleStoredRequest(t, f.state.Root, definition.ScheduleID)
			decoded, err := schedulecontract.DecodeRequest(stored)
			if err != nil || !reflect.DeepEqual(decoded, request) {
				t.Fatalf("SQLite did not persist raw pinned intent: %+v %v", decoded, err)
			}
			teamsScheduleAssertNoLaunch(t, f, strings.Repeat("b", 32))
		})
	}
}

func teamsScheduleAssertNoTaskEntries(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "tasks"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("schedule creation admitted task entries: %+v", entries)
	}
}

func teamsScheduleAssertPinnedRequest(t *testing.T, request schedulecontract.Request, catalogID string, options task.TaskConfig) {
	t.Helper()
	if request.Team == nil || request.Team.CatalogID != catalogID || request.Team.Team.TeamID != "review" || request.Team.MemberID != "primary" {
		t.Fatalf("schedule did not pin the requested catalog/Member: %+v", request)
	}
	if request.Provider != "" || request.Auto || !reflect.DeepEqual(request.Config, options) {
		t.Fatalf("Member settings replaced raw omissions: %+v", request)
	}
}

func TestTeamsScheduleNewOccurrencesUseLatestValidProviderBinding(t *testing.T) {
	for _, test := range []struct {
		name, savedModel, savedEffort string
		wantModel, wantEffort         string
	}{
		{"omitted options", "", "", "model-two", "low"},
		{"explicit model", "caller-model", "", "caller-model", "low"},
		{"explicit effort", "", "medium", "model-two", "medium"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newTeamsScheduleFixture(t)
			f.arguments.Config.Model, f.arguments.Config.Effort = test.savedModel, test.savedEffort
			definition := f.create(t)
			before, digest := teamsScheduleStoredRequest(t, f.state.Root, definition.ScheduleID)
			first, second := strings.Repeat("b", 32), strings.Repeat("c", 32)
			if _, err := f.dispatch(t, first); !errors.Is(err, errTeamsScheduleRefusal) {
				t.Fatalf("first occurrence did not reach finite provider refusal: %v", err)
			}
			old := f.captured()
			if len(old) != 1 || old[0].Provider != "alpha:run" || old[0].Team == nil || old[0].Team.TeamRevision != 1 {
				t.Fatalf("first actual request: %+v", old)
			}
			f.update(t, teamsScheduleDefinition("beta:run", "model-two", "low"))
			f.reopen(t)
			if _, err := f.dispatch(t, second); !errors.Is(err, errTeamsScheduleRefusal) {
				t.Fatalf("valid changed binding was rejected before preparation: %v", err)
			}
			captured := f.captured()
			teamsScheduleAssertLatestRequest(t, captured, test.wantModel, test.wantEffort)
			if !reflect.DeepEqual(old[0], captured[0]) {
				t.Fatal("new occurrence modified the earlier actual request")
			}
			after, currentDigest := teamsScheduleStoredRequest(t, f.state.Root, definition.ScheduleID)
			if !bytes.Equal(before, after) || currentDigest != digest {
				t.Fatal("catalog edit or occurrence rewrote the immutable schedule")
			}
			teamsScheduleAssertNoLaunch(t, f, first, second)
		})
	}
}

func teamsScheduleAssertLatestRequest(t *testing.T, captured []task.TaskRecord, model, effort string) {
	t.Helper()
	if len(captured) != 2 {
		t.Fatalf("expected two explicitly requested attempts, got %d", len(captured))
	}
	latest := captured[1]
	if latest.Provider != "beta:run" || latest.RequestedConfig.Model != model || latest.RequestedConfig.Effort != effort || latest.Team == nil || latest.Team.TeamRevision != 2 {
		t.Fatalf("latest settings/saved overrides not used: %+v", latest)
	}
}

func TestTeamsScheduleSameNamesInAnotherCatalogCannotRetarget(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	f.create(t)
	otherPath := filepath.Join(t.TempDir(), "catalog-b", "teams.sqlite")
	other := teams.New(otherPath, nil)
	_, id, err := other.Register(t.Context(), f.ref, teamsScheduleDefinition("beta:run", "other-model", "low"))
	if err != nil || id == f.catalogID {
		t.Fatalf("independent catalog fixture: %q %v", id, err)
	}
	f.deps.TeamCatalogPath = otherPath
	state, err := f.dispatch(t, strings.Repeat("b", 32))
	if !errors.Is(err, teams.ErrCatalogIdentity) || state.Release == nil || state.Release.Kind != schedulecontract.ReleaseNotCreated || len(f.captured()) != 0 {
		t.Fatalf("pinned schedule silently retargeted: %+v %v", state, err)
	}
	teamsScheduleAssertNoLaunch(t, f, strings.Repeat("b", 32))
}

func TestTeamsScheduleDeletedTargetDoesNotFallBackOrWriteDefault(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	f.create(t)
	project := task.TeamProject{Kind: "directory", Path: teamsScheduleCanonicalDirectory(t, f.arguments.Cwd)}
	backup := teamsScheduleDefinition("beta:run", "fallback-model", "low")
	backup.ID = "backup"
	backupRef := task.TeamReference{Scope: "user", TeamID: backup.ID}
	if _, _, err := f.catalog.Register(t.Context(), backupRef, backup); err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.SetDefault(t.Context(), project, backupRef); err != nil {
		t.Fatal(err)
	}
	if _, err := f.catalog.Delete(t.Context(), f.ref, 1); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.deps.TeamCatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.dispatch(t, strings.Repeat("b", 32))
	if !errors.Is(err, teams.ErrNotFound) || len(f.captured()) != 0 || state.Release == nil || state.Release.Kind != schedulecontract.ReleaseNotCreated {
		t.Fatalf("deleted target selected a fallback: %+v %v", state, err)
	}
	after, err := os.ReadFile(f.deps.TeamCatalogPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed scheduled selection wrote catalog/default state: %v", err)
	}
	result, err := f.catalog.Default(t.Context(), project)
	if err != nil || result.Status != "resolved" || result.Reference.TeamID != backup.ID {
		t.Fatalf("explicit preference changed: %+v %v", result, err)
	}
}

func TestTeamsScheduleUnboundMemberFailsWithoutAutomaticFallback(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	f.create(t)
	definition := teamsScheduleDefinition("beta:run", "model-two", "low")
	definition.Members[0].ExecutionDefaults = nil
	f.update(t, definition)
	state, err := f.dispatch(t, strings.Repeat("b", 32))
	if err == nil || errors.Is(err, errTeamsScheduleRefusal) || len(f.captured()) != 0 || state.Release == nil || state.Release.Kind != schedulecontract.ReleaseNotCreated {
		t.Fatalf("inadmissible unbound occurrence auto-selected a provider: %+v %v", state, err)
	}
}

func TestTeamsScheduleNewBindingConflictingWithSavedAutoFails(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	unbound := teamsScheduleDefinition("alpha:run", "unused", "high")
	unbound.Members[0].ExecutionDefaults = nil
	f.update(t, unbound)
	f.arguments.Auto = true
	definition := f.create(t)
	if !definition.Request.Auto || definition.Request.Provider != "" {
		t.Fatalf("auto caller intent changed: %+v", definition.Request)
	}
	f.update(t, teamsScheduleDefinition("beta:run", "newly-bound", "low"))
	state, err := f.dispatch(t, strings.Repeat("b", 32))
	if err == nil || errors.Is(err, errTeamsScheduleRefusal) || len(f.captured()) != 0 || state.Release == nil {
		t.Fatalf("binding/auto conflict was resolved by silent fallback: %+v %v", state, err)
	}
}

func TestTeamsScheduleSavedProviderOverrideDropsOtherBindingDefaults(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	f.arguments.Provider = "alpha:run"
	f.create(t)
	f.update(t, teamsScheduleDefinition("beta:run", "must-not-inherit", "high"))
	if _, err := f.dispatch(t, strings.Repeat("b", 32)); !errors.Is(err, errTeamsScheduleRefusal) {
		t.Fatalf("explicit provider did not reach preparation: %v", err)
	}
	requests := f.captured()
	if len(requests) != 1 || requests[0].Provider != "alpha:run" || requests[0].RequestedConfig.Model != "" || requests[0].RequestedConfig.Effort != "" {
		t.Fatalf("saved provider inherited another binding's options: %+v", requests)
	}
}

func TestTeamsScheduleFailedOccurrenceRemainsOneClaimWithoutReinvocation(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	definition := f.create(t)
	id := strings.Repeat("b", 32)
	if _, err := f.dispatch(t, id); !errors.Is(err, errTeamsScheduleRefusal) {
		t.Fatalf("fixture refusal was not reached: %v", err)
	}
	occurrence, err := f.store.GetOccurrence(t.Context(), id)
	if err != nil || occurrence.Release == nil || occurrence.Release.Kind != schedulecontract.ReleaseNotCreated {
		t.Fatalf("failed attempt retained no durable release: %+v %v", occurrence, err)
	}
	for range 2 {
		if observed, observeErr := f.tasks().Observe(id); observeErr != nil || observed.Release == nil {
			t.Fatalf("passive observation of refused attempt: %+v %v", observed, observeErr)
		}
	}
	repeated, err := f.store.ClaimAndArm(t.Context(), schedulecontract.Claim{
		ScheduleID:     definition.ScheduleID,
		ExpectedCursor: definition.CreatedAt, Due: occurrence.Due, ArmedAt: occurrence.ArmedAt, TaskID: strings.Repeat("c", 32),
	})
	if err != nil || repeated.AuthorizesInvocation() || repeated.TaskID != id {
		t.Fatalf("duplicate due gained another invocation: %+v %v", repeated, err)
	}
	history, err := f.store.History(t.Context(), schedulecontract.HistoryPage{ScheduleID: definition.ScheduleID, Limit: 10})
	if err != nil || len(history.Entries) != 1 || len(f.captured()) != 1 {
		t.Fatalf("failed occurrence was retried: history=%+v calls=%d err=%v", history, len(f.captured()), err)
	}
}

func TestTeamsScheduleSavedNonGitProjectWorksWithoutExplicitSchedulerFlag(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("Git is required to verify non-Git context:", err)
	}
	f.deps.Environment = []string{"PATH=" + filepath.Dir(git), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	project := task.TeamProject{Kind: "directory", Path: teamsScheduleCanonicalDirectory(t, f.arguments.Cwd)}
	f.ref = task.TeamReference{Scope: "project", TeamID: "review", Project: &project}
	if _, _, registerErr := f.catalog.Register(t.Context(), f.ref, teamsScheduleDefinition("alpha:run", "local-model", "high")); registerErr != nil {
		t.Fatal(registerErr)
	}
	f.arguments.TeamScope, f.arguments.ProjectRoot = "project", project.Path
	definition := f.create(t)
	if definition.Request.Team == nil || definition.Request.Team.Team.Project == nil || *definition.Request.Team.Team.Project != project {
		t.Fatalf("non-Git source context was not persisted: %+v", definition.Request)
	}
	if f.tasks().arguments.ProjectRoot != "" {
		t.Fatal("scheduler fixture accidentally supplied an explicit project-root")
	}
	f.reopen(t)
	if _, dispatchErr := f.dispatch(t, strings.Repeat("b", 32)); !errors.Is(dispatchErr, errTeamsScheduleRefusal) {
		t.Fatalf("saved project root was not used by admission: %v", dispatchErr)
	}
	captured := f.captured()
	if len(captured) != 1 || captured[0].Team == nil || *captured[0].Team.Target.Team.Project != project || captured[0].RequestedConfig.Model != "local-model" {
		t.Fatalf("scheduled local selection did not reach actual request: %+v", captured)
	}
	result, err := f.catalog.Default(t.Context(), project)
	if err != nil || result.Status != "unset" {
		t.Fatalf("scheduled selection implicitly assigned a default: %+v %v", result, err)
	}
}

func TestTeamsScheduleSameIDReplayKeepsRequestAfterCatalogDeletion(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	f.persistTasks(t)
	definition := f.create(t)
	id := strings.Repeat("b", 32)
	// This is the finite queue owner's persisted row, not provider start proof.
	writeRunnerStatus(t, f.statusPath, f.state.RootID, id, 7)
	first, dispatchErr := f.dispatch(t, id)
	if dispatchErr != nil || first.Observation.Admission != task.AdmissionAdmitted || first.Release != nil {
		t.Fatalf("finite ordinary queue submission did not reconcile: %+v %v", first, dispatchErr)
	}
	path := filepath.Join(f.state.Root, "tasks", id, "task.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.update(t, teamsScheduleDefinition("beta:run", "changed-model", "low"))
	if removeErr := os.Remove(f.deps.TeamCatalogPath); removeErr != nil {
		t.Fatal(removeErr)
	}
	replayed, replayErr := f.tasks().Dispatch(schedulerruntime.DispatchRequest{TaskID: id, Definition: definition})
	if replayErr != nil || replayed.Observation.Admission != task.AdmissionAdmitted || replayed.Release != nil {
		t.Fatalf("same-ID replay failed after deleting the catalog: %+v %v", replayed, replayErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("same-ID replay rewrote winning request: %v", err)
	}
	teamsScheduleAssertReplaySelection(t, after, f.captured())
	assertOrdinarySubmissionCommandCount(t, f.commandLog, "add", 1)
	assertOrdinarySubmissionCommandCount(t, f.commandLog, "enqueue", 1)
	teamsScheduleAssertNoLaunch(t, f, id)
}

func teamsScheduleAssertReplaySelection(t *testing.T, data []byte, captured []task.TaskRecord) {
	t.Helper()
	var saved task.TaskRecord
	if err := task.DecodeStrict(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Provider != "alpha:run" || saved.Team == nil || saved.Team.TeamRevision != 1 || saved.RequestedConfig.Model != "model-one" {
		t.Fatalf("old request lost admitted identity/settings: %+v", saved)
	}
	if len(captured) == 0 {
		t.Fatal("no actual request reached provider preparation")
	}
	for _, request := range captured {
		if request.Provider != saved.Provider || !reflect.DeepEqual(request.Team, saved.Team) {
			t.Fatalf("replay re-resolved Member settings: %+v", request)
		}
	}
}

// These bytes and digests come from the frozen pre-Team scheduler-v1 fixture,
// independently of the current writer/codec.
const (
	teamsScheduleLegacyRequest = "{\"auto\":true,\"canonical_cwd\":\"/tmp/TestExportPreChangeScheduledFixture3902220496/002\",\"config\":{}}\n"
	teamsScheduleLegacyDigest  = "4c62b030ec8f30739950c7fa14b855c152762ac9ea088f5ebecdce8712a5de5d"
)

type teamsScheduleLegacyDecoder struct {
	Provider     string          `json:"provider,omitempty"`
	Auto         bool            `json:"auto,omitempty"`
	CanonicalCwd string          `json:"canonical_cwd"`
	Config       task.TaskConfig `json:"config"`
}

func TestTeamsScheduleUpgradedRestartReadsLegacyAndTargetedRows(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	root := materializeLegacySchedule(t)
	before, digest := teamsScheduleStoredRequest(t, root, scheduleTestID)
	if string(before) != teamsScheduleLegacyRequest || digest != teamsScheduleLegacyDigest {
		t.Fatal("frozen pre-Team fixture bytes/digest changed")
	}
	state, err := openStore(root, f.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	f.state, f.arguments.Root, f.arguments.ScheduleID = state, root, strings.Repeat("e", 32)
	newDefinition := f.create(t)
	newBytes, _ := teamsScheduleStoredRequest(t, root, newDefinition.ScheduleID)
	teamsScheduleAssertLegacyDecoder(t, before, newBytes)
	f.reopen(t)
	old, err := f.store.GetSchedule(t.Context(), scheduleTestID)
	if err != nil || old.Definition.Request.Team != nil {
		t.Fatalf("upgraded restart rejected legacy row: %+v %v", old, err)
	}
	encoded, err := schedulecontract.MarshalRequest(old.Definition.Request)
	if err != nil || string(encoded) != teamsScheduleLegacyRequest {
		t.Fatalf("new codec changed legacy serialization: %q %v", encoded, err)
	}
	current, err := f.store.GetSchedule(t.Context(), newDefinition.ScheduleID)
	if err != nil || !reflect.DeepEqual(current.Definition, newDefinition) {
		t.Fatalf("upgraded restart rejected targeted row: %+v %v", current, err)
	}
	after, afterDigest := teamsScheduleStoredRequest(t, root, scheduleTestID)
	if !bytes.Equal(before, after) || afterDigest != teamsScheduleLegacyDigest {
		t.Fatal("adding/reopening a Team schedule rewrote old stored bytes/hash")
	}
}

func teamsScheduleAssertLegacyDecoder(t *testing.T, oldBytes, newBytes []byte) {
	t.Helper()
	var legacy teamsScheduleLegacyDecoder
	if err := task.DecodeStrict(oldBytes, &legacy); err != nil {
		t.Fatalf("legacy decoder rejected its supported request: %v", err)
	}
	current, err := schedulecontract.DecodeRequest(newBytes)
	if err != nil || current.Team == nil {
		t.Fatalf("new request is not a valid targeted-schema control: %+v %v", current, err)
	}
	if decodeErr := task.DecodeStrict(newBytes, &legacy); decodeErr == nil {
		t.Fatal("old strict decoder accepted the unsupported target field")
	}
	// DecodeStrict exposes rejection as an error without an unknown-field
	// sentinel. Removing only the added field must restore the old shape.
	control := teamsScheduleLegacyDecoder{
		Provider: current.Provider, Auto: current.Auto,
		CanonicalCwd: current.CanonicalCwd, Config: current.Config,
	}
	withoutTarget, err := task.MarshalCanonical(control)
	if err != nil {
		t.Fatal(err)
	}
	if decodeErr := task.DecodeStrict(withoutTarget, &legacy); decodeErr != nil {
		t.Fatalf("legacy decoder rejected the same request without its new field: %v", decodeErr)
	}
}
