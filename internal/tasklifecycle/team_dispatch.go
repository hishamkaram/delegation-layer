package tasklifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func resolveDispatchTeam(request DispatchRequest, options DispatchOptions, store *taskdir.Store, id, cwd string) (DispatchRequest, error) {
	return resolveDispatchTeamContext(context.Background(), request, options, store, id, cwd)
}

func resolveDispatchTeamContext(ctx context.Context, request DispatchRequest, options DispatchOptions, store *taskdir.Store, id, cwd string) (DispatchRequest, error) {
	if request.TeamTarget == nil {
		return request, task.ValidateTeamContext(request.TeamContext)
	}
	if request.ResumeTask != "" {
		return request, fmt.Errorf("%w: continuation inherits its recorded Team selection", task.ErrInvalidTeamSelection)
	}
	old, exists, err := recordedTeamRequest(store, id)
	if err != nil {
		return request, err
	}
	if exists {
		if err = validateTeamReplay(request, old); err != nil {
			return request, err
		}
		request.Provider, request.Auto, request.Config = old.Provider, false, old.RequestedConfig
		request.TeamContext, request.TeamTarget = old.Team.Clone(), nil
		return request, nil
	}
	if options.Members == nil {
		return request, fmt.Errorf("%w: Member resolver unavailable", task.ErrInvalidTeamSelection)
	}
	ctx, cancel := context.WithTimeout(ctx, workspace.DefaultBudget)
	defer cancel()
	source, err := teamSourceCwd(ctx, store, request, cwd)
	if err != nil {
		return request, err
	}
	member, err := options.Members(ctx, request.TeamTarget.Clone(), store.Root, source, request.ProjectRoot)
	if err != nil {
		return request, err
	}
	intent := task.TeamIntent{Provider: request.Provider, Auto: request.Auto, Config: request.Config}
	resolved, err := teamcontract.MergeExecution(member.Member.ExecutionDefaults, intent)
	if err != nil {
		return request, err
	}
	selection, err := teamcontract.SelectionContext(member, intent)
	if err != nil {
		return request, err
	}
	request.Provider, request.Auto, request.Config = resolved.Provider, resolved.Auto, resolved.Config
	request.TeamContext = selection
	return request, nil
}

func recordedTeamRequest(store *taskdir.Store, id string) (record *task.TaskRecord, exists bool, resultErr error) {
	td, err := store.OpenTask(id)
	if errors.Is(err, os.ErrNotExist) {
		present, inspectErr := store.TaskEntryExists(id)
		if inspectErr != nil || present {
			return nil, present, errors.Join(task.ErrEvidenceFault, err, inspectErr)
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	record, err = td.RecordedRequest()
	return record, true, err
}

func teamSourceCwd(ctx context.Context, store *taskdir.Store, request DispatchRequest, cwd string) (source string, resultErr error) {
	if request.WorkspaceID == "" {
		return cwd, nil
	}
	resource, err := workspace.LoadResource(ctx, store, request.WorkspaceID)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	return resource.Request().SourceCwd, nil
}

func validateTeamReplay(request DispatchRequest, old *task.TaskRecord) error {
	if old == nil || old.Team == nil || request.TeamTarget == nil ||
		!sameTeamTarget(*request.TeamTarget, old.Team.Target) {
		return task.ErrRequestConflict
	}
	if request.ProjectRoot != "" {
		project := old.Team.Target.Team.Project
		root, err := config.CanonicalizePath(request.ProjectRoot)
		if err != nil {
			return err
		}
		if project == nil || project.Kind != "directory" || root != project.Path {
			return task.ErrRequestConflict
		}
	}
	intent := task.TeamIntent{Provider: request.Provider, Auto: request.Auto, Config: request.Config}
	return compareTeamIntent(intent, old.Team.Intent, old.Provider, old.BudgetNanos)
}

func sameTeamTarget(requested, saved task.TeamTarget) bool {
	if requested.MemberID != saved.MemberID || requested.Team.Scope != saved.Team.Scope || requested.Team.TeamID != saved.Team.TeamID {
		return false
	}
	if requested.CatalogID != "" && requested.CatalogID != saved.CatalogID {
		return false
	}
	return requested.Team.Project == nil || reflect.DeepEqual(requested.Team.Project, saved.Team.Project)
}

func compareTeamIntent(requested, saved task.TeamIntent, provider string, budgetNanos int64) error {
	if requested.Config.Budget != "" {
		budget, err := config.ParseBudgetForProvider(provider, requested.Config.Budget)
		if err != nil {
			return err
		}
		if int64(budget) != budgetNanos {
			return task.ErrRequestConflict
		}
	}
	requested.Config.Budget, saved.Config.Budget = "", ""
	for _, intent := range []*task.TeamIntent{&requested, &saved} {
		// Reuse task admission's duration and budget rules. Caller intent keeps
		// omission distinct, while equivalent explicit durations compare equal.
		normalized := task.TaskRecord{
			Provider: provider, BudgetNanos: budgetNanos,
			RequestedConfig: task.TaskConfig{NativeTimeout: intent.Config.NativeTimeout},
		}
		if err := task.NormalizeRequestedConfig(&normalized); err != nil {
			return err
		}
		intent.Config.NativeTimeout = normalized.RequestedConfig.NativeTimeout
	}
	// Permission omission has the existing read-only meaning. Unlike Member
	// defaults, this constant does not change when the catalog is edited.
	if requested.Config.Permission == "" {
		requested.Config.Permission = config.ModeReadOnly
	}
	if saved.Config.Permission == "" {
		saved.Config.Permission = config.ModeReadOnly
	}
	if !reflect.DeepEqual(requested, saved) {
		return task.ErrRequestConflict
	}
	return nil
}

// rebaseTeamRequest validates original intent and non-default identity before
// adopting a concurrently persisted winner. It never changes the stored record.
func rebaseTeamRequest(candidate *task.TaskRecord, old *task.TaskRecord) error {
	if candidate.Team == nil || old == nil || old.Team == nil ||
		!sameTeamTarget(candidate.Team.Target, old.Team.Target) ||
		candidate.CanonicalCwd != old.CanonicalCwd || candidate.BriefSHA256 != old.BriefSHA256 ||
		candidate.BriefLength != old.BriefLength || !reflect.DeepEqual(candidate.Workspace, old.Workspace) ||
		!reflect.DeepEqual(candidate.PriorSession, old.PriorSession) {
		return task.ErrRequestConflict
	}
	if err := compareTeamIntent(candidate.Team.Intent, old.Team.Intent, old.Provider, old.BudgetNanos); err != nil {
		return err
	}
	*candidate = *old
	candidate.Team = old.Team.Clone()
	return nil
}

func validatePreparedTeamProject(request DispatchRequest, options DispatchOptions, cwd string) error {
	return validatePreparedTeamProjectContext(context.Background(), request, options, cwd)
}

func validatePreparedTeamProjectContext(ctx context.Context, request DispatchRequest, options DispatchOptions, cwd string) error {
	if request.TeamTarget == nil || request.TeamContext == nil || request.TeamContext.Target.Team.Scope != "project" {
		return nil
	}
	if options.CheckTeamProject == nil {
		return fmt.Errorf("%w: project validator unavailable", task.ErrInvalidTeamSelection)
	}
	ctx, cancel := context.WithTimeout(ctx, workspace.DefaultBudget)
	defer cancel()
	return options.CheckTeamProject(ctx, request.TeamContext.Target.Team, cwd, request.ProjectRoot)
}
