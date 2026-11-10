package tasklifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

type teamDispatchFixture struct {
	store   *taskdir.Store
	td      *taskdir.TaskDir
	request *task.TaskRecord
	meta    *task.MetaRecord
	brief   []byte
}

func newTeamDispatchFixture(t *testing.T) teamDispatchFixture {
	t.Helper()
	store := newResourceGateStore(t)
	template, original := testsupport.NewAppTaskInStore(t, store, strings.Repeat("a", 32), false, nil)
	t.Cleanup(func() {
		if err := template.Close(); err != nil {
			t.Error(err)
		}
	})
	_, meta, err := template.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID, meta.TaskID = strings.Repeat("b", 32), strings.Repeat("b", 32)
	request.RequestedConfig.Model, request.RequestedConfig.Effort = "member-model-v1", "member-effort-v1"
	meta.RequestedConfig = request.RequestedConfig
	meta.Environment, meta.EnvironmentRecorded = []string{}, true
	request.Team = &task.TeamContext{
		SchemaVersion: 1, TeamRevision: 3, TeamName: "Review team", TeamPurpose: "Review changes.", MemberName: "Primary",
		Target: task.TeamTarget{
			Team:     task.TeamReference{Scope: "project", TeamID: "review-team", Project: &task.TeamProject{Kind: "directory", Path: request.CanonicalCwd}},
			MemberID: "primary", CatalogID: "0123456789abcdef0123456789abcdef",
		},
		Roles:  []task.TeamRoleContext{{ID: "reviewer", Name: "Reviewer", Responsibility: "Check correctness."}},
		Intent: task.TeamIntent{Config: task.TaskConfig{Permission: "read-only"}},
	}
	brief := []byte("brief")
	td, err := store.CreateTask(request.TaskID, &request, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return teamDispatchFixture{store: store, td: td, request: &request, meta: meta, brief: brief}
}

func (fixture teamDispatchFixture) replay() DispatchRequest {
	target := fixture.request.Team.Target.Clone()
	intent := fixture.request.Team.Intent
	return DispatchRequest{
		Root: fixture.store.Root, TaskID: fixture.request.TaskID, Cwd: fixture.request.CanonicalCwd, BriefBytes: append([]byte(nil), fixture.brief...),
		TeamTarget: &target, Provider: intent.Provider, Auto: intent.Auto, Config: intent.Config,
	}
}

func TestTeamReplayUsesPersistedRequestWithoutCatalog(t *testing.T) {
	for _, state := range []string{"definition edited", "definition deleted", "metadata not yet committed"} {
		t.Run(state, func(t *testing.T) {
			fixture := newTeamDispatchFixture(t)
			if state == "metadata not yet committed" {
				if err := os.Remove(filepath.Join(fixture.td.Dir, "meta.json")); err != nil {
					t.Fatal(err)
				}
			}
			lookups := 0
			options := DispatchOptions{Members: func(context.Context, task.TeamTarget, string, string, string) (teamcontract.ResolvedMember, error) {
				lookups++
				return teamcontract.ResolvedMember{}, errors.New(state)
			}}
			resolved, err := resolveDispatchTeam(fixture.replay(), options, fixture.store, fixture.request.TaskID, fixture.request.CanonicalCwd)
			if err != nil || lookups != 0 {
				t.Fatalf("persisted replay consulted mutable catalog: lookups=%d, err=%v", lookups, err)
			}
			if resolved.Provider != fixture.request.Provider || resolved.Auto || !reflect.DeepEqual(resolved.Config, fixture.request.RequestedConfig) || !reflect.DeepEqual(resolved.TeamContext, fixture.request.Team) {
				t.Fatalf("replay changed admitted settings: %+v", resolved)
			}
			resolved.TeamContext.Roles[0].Responsibility = "Caller mutation."
			persisted, err := fixture.td.RecordedRequest()
			if err != nil || !reflect.DeepEqual(persisted, fixture.request) {
				t.Fatalf("replay mutation reached persisted request: %+v, %v", persisted, err)
			}
		})
	}
}

func TestTeamSelectionPinCannotRetargetReplayOrUnboundTask(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	request := fixture.replay()
	request.ExpectedTeamSelection = &teamcontract.Selection{Execution: teamcontract.Execution{Provider: "other:run"}}
	resolved, err := resolveDispatchTeam(request, DispatchOptions{}, fixture.store, request.TaskID, request.Cwd)
	if err != nil || resolved.Provider != fixture.request.Provider {
		t.Fatalf("preview pin retargeted recorded task: %+v, %v", resolved, err)
	}
	request.TeamTarget = nil
	if _, err = resolveDispatchTeam(request, DispatchOptions{}, fixture.store, request.TaskID, request.Cwd); !errors.Is(err, teamcontract.ErrTeamSelectionChanged) {
		t.Fatalf("pin without target: %v", err)
	}
}

func TestTeamReplayRejectsDifferentExplicitIntentWithoutCatalog(t *testing.T) {
	changes := []struct {
		name string
		edit func(*DispatchRequest)
	}{
		{"member", func(r *DispatchRequest) { r.TeamTarget.MemberID = "secondary" }},
		{"team", func(r *DispatchRequest) { r.TeamTarget.Team.TeamID = "other-team" }},
		{"catalog", func(r *DispatchRequest) { r.TeamTarget.CatalogID = strings.Repeat("f", 32) }},
		{"project", func(r *DispatchRequest) { r.TeamTarget.Team.Project.Path = filepath.Dir(r.Cwd) }},
		{"provider", func(r *DispatchRequest) { r.Provider = "fixture:test" }},
		{"auto", func(r *DispatchRequest) { r.Auto = true }},
		{"explicit inherited model", func(r *DispatchRequest) { r.Config.Model = "member-model-v1" }},
		{"effort", func(r *DispatchRequest) { r.Config.Effort = "experimental" }},
		{"permission", func(r *DispatchRequest) { r.Config.Permission = "workspace-write" }},
		{"budget", func(r *DispatchRequest) { r.Config.Budget = "2m" }},
	}
	fixture := newTeamDispatchFixture(t)
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			request := fixture.replay()
			change.edit(&request)
			options := DispatchOptions{Members: func(context.Context, task.TeamTarget, string, string, string) (teamcontract.ResolvedMember, error) {
				t.Error("conflicting replay consulted catalog")
				return teamcontract.ResolvedMember{}, task.ErrInvalidTeamSelection
			}}
			if _, err := resolveDispatchTeam(request, options, fixture.store, fixture.request.TaskID, request.Cwd); !errors.Is(err, task.ErrRequestConflict) {
				t.Fatalf("different explicit intent error = %v, want ErrRequestConflict", err)
			}
		})
	}
	request := fixture.replay()
	request.Config.Budget = "60s"
	if _, err := resolveDispatchTeam(request, DispatchOptions{}, fixture.store, fixture.request.TaskID, request.Cwd); err != nil {
		t.Fatalf("semantically equal explicit budget rejected: %v", err)
	}
}

func TestTeamInvalidTargetRefusesBeforeManagedProvisioning(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	request := fixture.replay()
	request.TaskID, request.Isolation = strings.Repeat("c", 32), "git-worktree"
	request.Selection.RunnerOwnership = task.RunnerOwnershipManaged
	request.TeamTarget.MemberID = ""
	lookups := 0
	options := DispatchOptions{OpenStore: func(root string, _ bool) (*taskdir.Store, error) {
		return taskdir.OpenStore(root)
	}, Members: func(_ context.Context, target task.TeamTarget, root, cwd, projectRoot string) (teamcontract.ResolvedMember, error) {
		lookups++
		if root != fixture.store.Root || cwd != fixture.request.CanonicalCwd || projectRoot != "" {
			t.Errorf("resolver received the wrong context: %q %q %q", root, cwd, projectRoot)
		}
		return teamcontract.ResolvedMember{}, task.ValidateTeamTarget(target, false)
	}}
	result := Dispatch(request, options)
	if !errors.Is(result.Err(), task.ErrInvalidTeamSelection) || result.Phase != DispatchValidate || lookups != 1 || result.Workspace != nil || result.Resources != nil {
		t.Fatalf("invalid target reached later admission: phase=%v, lookups=%d, err=%v", result.Phase, lookups, result.Err())
	}
	if present, err := fixture.store.TaskEntryExists(request.TaskID); err != nil || present {
		t.Fatalf("invalid target created a task entry: present=%t, %v", present, err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.store.Root, "workspaces"))
	if err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) != 0 {
		t.Fatalf("invalid target created workspace resources: %v, %v", entries, err)
	}
}

func TestTeamReplayTreatsOmittedPermissionAsReadOnly(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	request := fixture.replay()
	request.Config.Permission, request.Config.Budget = "", "60s"
	if _, err := resolveDispatchTeam(request, DispatchOptions{}, fixture.store, fixture.request.TaskID, request.Cwd); err != nil {
		t.Fatalf("omitted permission and equivalent budget conflicted with persisted request: %v", err)
	}
	saved := task.TeamIntent{}
	requested := task.TeamIntent{Config: task.TaskConfig{Permission: "read-only", Budget: "60s"}}
	if err := compareTeamIntent(requested, saved, fixture.request.Provider, int64(time.Minute)); err != nil {
		t.Fatalf("CLI default conflicted with saved schedule omissions: %v", err)
	}
	requested.Config.Permission = "workspace-write"
	if err := compareTeamIntent(requested, saved, fixture.request.Provider, int64(time.Minute)); !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("different explicit permission error = %v, want ErrRequestConflict", err)
	}
}

func TestTeamReplayComparesNativeTimeoutByDurationWithoutCatalog(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	saved := *fixture.request
	saved.Team = saved.Team.Clone()
	saved.TaskID = strings.Repeat("d", 32)
	saved.RequestedConfig.NativeTimeout = "1m0s"
	saved.Team.Intent.Config.NativeTimeout = "60s"
	meta := *fixture.meta
	meta.TaskID, meta.RequestedConfig = saved.TaskID, saved.RequestedConfig
	td, err := fixture.store.CreateTask(saved.TaskID, &saved, fixture.brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	for _, tc := range []struct {
		value string
		valid bool
	}{{"1m", true}, {"60s", true}, {"30s", false}, {"", false}, {"invalid", false}} {
		t.Run(tc.value, func(t *testing.T) {
			request := fixture.replay()
			request.TaskID, request.Config.NativeTimeout = saved.TaskID, tc.value
			resolved, resolveErr := resolveDispatchTeam(request, DispatchOptions{}, fixture.store, saved.TaskID, request.Cwd)
			if tc.valid {
				if resolveErr != nil || !reflect.DeepEqual(resolved.Config, saved.RequestedConfig) {
					t.Fatalf("equivalent replay changed saved settings: %+v, %v", resolved.Config, resolveErr)
				}
			} else if resolveErr == nil {
				t.Fatalf("different or invalid native timeout %q accepted", tc.value)
			}
		})
	}
}

func TestTeamDispatchRejectsExplicitTargetOnResumeBeforeCatalog(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	request := fixture.replay()
	request.TaskID, request.ResumeTask = strings.Repeat("c", 32), fixture.request.TaskID
	lookups := 0
	options := DispatchOptions{Members: func(context.Context, task.TeamTarget, string, string, string) (teamcontract.ResolvedMember, error) {
		lookups++
		return teamcontract.ResolvedMember{}, errors.New("resume reached catalog")
	}}
	if _, err := resolveDispatchTeam(request, options, fixture.store, request.TaskID, request.Cwd); !errors.Is(err, task.ErrInvalidTeamSelection) || lookups != 0 {
		t.Fatalf("resume with explicit target error = %v, lookups=%d; want invalid selection without catalog", err, lookups)
	}
}

func TestTeamReplayChecksExplicitProjectRootWithoutCatalog(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	for _, tc := range []struct {
		name, projectRoot string
		conflict          bool
	}{
		{"omitted", "", false},
		{"matching directory", fixture.request.CanonicalCwd, false},
		{"different directory", filepath.Join(fixture.request.CanonicalCwd, "other"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := fixture.replay()
			// Raw CLI targeting can omit Project; an explicit project-root must
			// still agree with the canonical persisted project identity.
			request.TeamTarget.Team.Project = nil
			request.ProjectRoot = tc.projectRoot
			_, err := resolveDispatchTeam(request, DispatchOptions{}, fixture.store, request.TaskID, request.Cwd)
			if tc.conflict {
				if !errors.Is(err, task.ErrRequestConflict) {
					t.Fatalf("different explicit project-root error = %v, want ErrRequestConflict", err)
				}
			} else if err != nil {
				t.Fatalf("matching project context rejected without catalog: %v", err)
			}
		})
	}
	for _, scope := range []string{"git", "user"} {
		t.Run(scope+" rejects project-root", func(t *testing.T) {
			old := *fixture.request
			old.Team = fixture.request.Team.Clone()
			request := fixture.replay()
			request.ProjectRoot = fixture.request.CanonicalCwd
			if scope == "git" {
				old.Team.Target.Team.Project.Kind = "git"
				request.TeamTarget.Team.Project = nil
			} else {
				old.Team.Target.Team.Scope, old.Team.Target.Team.Project = "user", nil
				request.TeamTarget.Team = old.Team.Target.Team.Clone()
			}
			if err := validateTeamReplay(request, &old); !errors.Is(err, task.ErrRequestConflict) {
				t.Fatalf("inapplicable explicit project-root error = %v, want ErrRequestConflict", err)
			}
		})
	}
}

func TestTeamPreparedProjectCheckUsesProvisionedCwd(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	request := fixture.replay()
	request.TeamContext = fixture.request.Team.Clone()
	preparedCwd := filepath.Join(t.TempDir(), "checkout")
	request.ProjectRoot = fixture.request.CanonicalCwd
	checks := 0
	refusal := errors.New("project drift")
	options := DispatchOptions{CheckTeamProject: func(_ context.Context, reference task.TeamReference, cwd, projectRoot string) error {
		checks++
		if !reflect.DeepEqual(reference, fixture.request.Team.Target.Team) || cwd != preparedCwd || projectRoot != request.ProjectRoot {
			t.Error("project check did not receive saved scope and prepared workspace")
		}
		return refusal
	}}
	if err := validatePreparedTeamProject(request, options, preparedCwd); !errors.Is(err, refusal) || checks != 1 {
		t.Fatalf("prepared project check = %v, calls=%d", err, checks)
	}
	if err := validatePreparedTeamProject(request, DispatchOptions{}, preparedCwd); !errors.Is(err, task.ErrInvalidTeamSelection) {
		t.Fatalf("project target without checker error = %v", err)
	}
	request.TeamTarget = nil
	if err := validatePreparedTeamProject(request, options, preparedCwd); err != nil || checks != 1 {
		t.Fatalf("persisted replay reprobed project: %v, calls=%d", err, checks)
	}
}

func TestTeamContinuationInheritsContextAndAllowsExecutionOverrides(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	requested := DispatchRequest{Config: task.TaskConfig{Model: "explicit-model", Effort: "experimental", Budget: "30s"}}
	successor, err := BuildContinuationRequest(requested, fixture.store.Root, fixture.request, nil, fixture.brief)
	if err != nil {
		t.Fatal(err)
	}
	want := task.TaskConfig{Permission: "read-only", Model: "explicit-model", Effort: "experimental", Budget: "30s"}
	if !reflect.DeepEqual(successor.Config, want) || successor.TeamTarget != nil || !reflect.DeepEqual(successor.TeamContext, fixture.request.Team) {
		t.Fatalf("continuation lost admitted context or overrides: %+v", successor)
	}
	lookups := 0
	options := DispatchOptions{Members: func(context.Context, task.TeamTarget, string, string, string) (teamcontract.ResolvedMember, error) {
		lookups++
		return teamcontract.ResolvedMember{}, errors.New("catalog deleted")
	}}
	resolved, err := resolveDispatchTeam(successor, options, fixture.store, strings.Repeat("c", 32), successor.Cwd)
	if err != nil || lookups != 0 || !reflect.DeepEqual(resolved.TeamContext, fixture.request.Team) {
		t.Fatalf("inherited context caused a catalog lookup: calls=%d, err=%v", lookups, err)
	}
	successor.TeamContext.Target.Team.Project.Path = filepath.Join(successor.Cwd, "changed")
	successor.TeamContext.Roles[0].Name = "Changed successor role"
	persisted, err := fixture.td.RecordedRequest()
	if err != nil || !reflect.DeepEqual(persisted.Team, fixture.request.Team) {
		t.Fatalf("successor mutated predecessor context: %+v, %v", persisted, err)
	}
}

func TestTeamWorkspaceDigestIgnoresRevisionAndDefaultChanges(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	request := fixture.replay()
	request.TeamContext = fixture.request.Team.Clone()
	request.Provider, request.Config = fixture.request.Provider, fixture.request.RequestedConfig
	first, err := workspaces.DispatchDigest(WorkspaceDispatchRequest(request), request.TaskID, request.Cwd, fixture.brief, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.TeamContext.TeamRevision++
	request.TeamContext.MemberName = "Renamed member"
	request.Provider, request.Config.Model, request.Config.Effort = "future-agent:exec", "new-default", "new-effort"
	second, err := workspaces.DispatchDigest(WorkspaceDispatchRequest(request), request.TaskID, request.Cwd, fixture.brief, time.Minute, nil)
	if err != nil || second != first {
		t.Fatalf("mutable resolved values changed workspace intent identity: %s != %s, %v", first, second, err)
	}
}

type teamResumeCandidates struct {
	prepare commonprovider.PrepareCandidate
}

func (source teamResumeCandidates) Current(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	return source.prepare(request, environment)
}

func (teamResumeCandidates) ForTask(task.TaskRecord, *task.MetaRecord, []string) (commonprovider.ProfileCandidate, bool, error) {
	return commonprovider.ProfileCandidate{}, false, errors.New("unexpected historical profile preparation")
}

func teamResumeOptions(t *testing.T, fixture teamDispatchFixture) DispatchOptions {
	t.Helper()
	prepare := commonprovider.ReadyCandidate(func(request task.TaskRecord) (commonprovider.PreparedProfile, error) {
		return commonprovider.PreparedProfile{
			Plan:      execution.Plan{Executable: fixture.meta.ProviderExecutable, Directory: request.CanonicalCwd, Environment: []string{}, Predicate: task.FixturePredicateRef()},
			Effective: fixture.meta.EffectiveConfig,
		}, nil
	})
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{
		Description: commonprovider.Description{
			ID: "fixture:test", SupportedModes: []string{"read-only"}, Discoverable: true,
			SupportedOptions: []string{commonprovider.OptionModel, commonprovider.OptionEffort, commonprovider.OptionContinuation},
			Continuation:     commonprovider.ContinuationNative,
			ReadOnly:         commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
			Runtime:          commonprovider.RuntimeCapability{RequiredFlags: []string{"--fixture"}},
		},
		Prepare: prepare, Interpreters: []predicate.Interpreter{predicate.FixtureV1()},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := admissioncore.NewProfiles(teamResumeCandidates{prepare: prepare}, nil, []string{})
	options := DispatchOptions{Catalog: catalog, Creation: CreationOptions{Admission: admissioncore.PreparationOptions{Profiles: profiles}}}
	options.ContinuationProfiles = func(meta task.MetaRecord, _ []string) admissioncore.Profiles {
		if meta.TaskID != fixture.request.TaskID {
			t.Error("continuation selected another predecessor's profile")
		}
		return profiles
	}
	return options
}

func sealTeamPredecessor(t *testing.T, fixture teamDispatchFixture) {
	t.Helper()
	permit, err := fixture.td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			if releaseErr := permit.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		}
	}()
	for _, operation := range []func() error{
		permit.Consume,
		func() error { return fixture.td.RecordStarted(0) },
		func() error {
			return fixture.td.RecordProviderIdentity(task.SessionIdentity{Provider: "fixture:test", ConversationID: "recorded-conversation"})
		},
		func() error { return fixture.td.WriteRawFiles("answer\n", "") },
	} {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
	released = true
	if _, cleanup, err := fixture.td.Collect(task.FixturePredicateRef()); err != nil || cleanup != nil {
		t.Fatalf("predecessor did not reach committed terminal evidence: %v", errors.Join(err, cleanup))
	}
}

func TestTeamOrdinaryResumeInheritsPersistedContextWithoutCatalog(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	sealTeamPredecessor(t, fixture)
	options := teamResumeOptions(t, fixture)
	lookups := 0
	options.Members = func(context.Context, task.TeamTarget, string, string, string) (teamcontract.ResolvedMember, error) {
		lookups++
		return teamcontract.ResolvedMember{}, errors.New("catalog deleted")
	}
	config := task.TaskConfig{Permission: "read-only", Model: "explicit-successor-model", Effort: "experimental", Budget: "30s"}
	successor, err := admissioncore.BuildRequest(fixture.store.RootID, strings.Repeat("c", 32), fixture.request.Provider, fixture.request.CanonicalCwd, config, fixture.brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = prepareDispatchContinuation(fixture.store, options, &successor, fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if lookups != 0 || !reflect.DeepEqual(successor.Team, fixture.request.Team) || successor.RequestedConfig.Model != "explicit-successor-model" || successor.RequestedConfig.Effort != "experimental" {
		t.Fatalf("ordinary resume lost context/overrides or read catalog: %+v, lookups=%d", successor, lookups)
	}
	wantPrior := &task.PriorSession{Provider: "fixture:test", ConversationID: "recorded-conversation", PredecessorTaskID: fixture.request.TaskID}
	if !reflect.DeepEqual(successor.PriorSession, wantPrior) {
		t.Fatalf("resume prior session = %+v, want %+v", successor.PriorSession, wantPrior)
	}
	successor.Team.Roles[0].Responsibility = "Changed successor role."
	if !reflect.DeepEqual(fixture.request.Team.Roles, []task.TeamRoleContext{{ID: "reviewer", Name: "Reviewer", Responsibility: "Check correctness."}}) {
		t.Fatal("ordinary resume shared predecessor Team context")
	}
}
