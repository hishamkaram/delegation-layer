package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func resolveTeamsProject(ctx context.Context, cwd, projectRoot string, deps Dependencies) (task.TeamProject, error) {
	deps = deps.withCapturedEnvironment()
	git := workspace.Git{Runner: teamsProjectRunner{}, Environment: slices.Clone(deps.Environment)}
	return teams.ResolveProject(ctx, cwd, projectRoot, git)
}

type teamsProjectRunner struct{}

func (teamsProjectRunner) Run(ctx context.Context, command *exec.Cmd) error {
	ownerExecutable, err := os.Executable()
	if err != nil {
		return err
	}
	return execution.RunIsolatedCommand(ctx, ownerExecutable, command)
}

func catalogForTeams(deps Dependencies) (*teams.Catalog, error) {
	path, err := resolveTeamsCatalogPath(deps.withCapturedEnvironment())
	if err != nil {
		return nil, err
	}
	return teams.New(path, func(defaults contract.ExecutionDefaults) error {
		catalog := deps.Catalog
		if catalog.IsZero() {
			catalog = NativeCatalog()
		}
		if err := validateTeamsDefaults(catalog, defaults); err != nil {
			return &teamsInputError{cause: err}
		}
		return nil
	}), nil
}

func lookupTeamMember(ctx context.Context, target task.TeamTarget, root, cwd, projectRoot string, deps Dependencies) (contract.ResolvedMember, error) {
	deps = deps.withCapturedEnvironment()
	path, err := resolveTeamsCatalogPath(deps)
	if err != nil {
		return contract.ResolvedMember{}, err
	}
	canonicalRoot, err := config.CanonicalizePath(root)
	if err != nil {
		return contract.ResolvedMember{}, err
	}
	if config.PathsOverlap(canonicalRoot, path) {
		return contract.ResolvedMember{}, fmt.Errorf("%w: execution root and Team catalog must not overlap", task.ErrInvalidTeamSelection)
	}
	deps.TeamCatalogPath = path
	target = target.Clone()
	if target.Team.Scope == "project" {
		project, projectErr := projectForTeamReference(ctx, target.Team, cwd, projectRoot, deps)
		if projectErr != nil {
			return contract.ResolvedMember{}, projectErr
		}
		target.Team.Project = &project
	}
	if err = task.ValidateTeamTarget(target, false); err != nil {
		return contract.ResolvedMember{}, &teamsInputError{cause: err}
	}
	catalog, err := catalogForTeams(deps)
	if err != nil {
		return contract.ResolvedMember{}, err
	}
	return catalog.ResolveMember(ctx, target)
}

func checkTeamProject(ctx context.Context, ref task.TeamReference, cwd, projectRoot string, deps Dependencies) error {
	if err := task.ValidateTeamReference(ref); err != nil {
		return err
	}
	if ref.Scope == "user" {
		return nil
	}
	_, err := projectForTeamReference(ctx, ref, cwd, projectRoot, deps.withCapturedEnvironment())
	return err
}

func projectForTeamReference(ctx context.Context, ref task.TeamReference, cwd, projectRoot string, deps Dependencies) (task.TeamProject, error) {
	if ref.Project != nil && ref.Project.Kind == "directory" && projectRoot == "" {
		projectRoot = ref.Project.Path
	}
	project, err := resolveTeamsProject(ctx, cwd, projectRoot, deps)
	if err != nil {
		return task.TeamProject{}, err
	}
	if ref.Project != nil && *ref.Project != project {
		return task.TeamProject{}, teams.ErrProjectMismatch
	}
	return project, nil
}

// resolveTeamsCatalogPath uses only the command's captured environment. The
// caller keeps the result in TeamCatalogPath for all later consumers.
func resolveTeamsCatalogPath(deps Dependencies) (string, error) {
	if deps.TeamCatalogPath != "" {
		return config.CanonicalizePath(deps.TeamCatalogPath)
	}
	if deps.Environment == nil {
		return "", errors.New("team catalog resolution requires a captured environment")
	}
	value := func(key string) string { return verifiedexec.EnvironmentValue(deps.Environment, key) }
	var directory string
	switch runtime.GOOS {
	case "darwin", "ios":
		if value("HOME") != "" {
			directory = filepath.Join(value("HOME"), "Library", "Application Support")
		}
	case "windows":
		directory = value("AppData")
	case "plan9":
		if value("home") != "" {
			directory = filepath.Join(value("home"), "lib")
		}
	default:
		directory = value("XDG_CONFIG_HOME")
		if directory == "" && value("HOME") != "" {
			directory = filepath.Join(value("HOME"), ".config")
		}
	}
	if directory == "" || !filepath.IsAbs(directory) {
		return "", errors.New("captured user configuration directory is missing or relative; cannot locate Team catalog")
	}
	return config.CanonicalizePath(filepath.Join(directory, "delegation-layer-catalog", "teams.sqlite"))
}

// Registration checks only supplied capabilities. Required per-task options
// may be filled by the caller, and model discovery is deliberately advisory.
func validateTeamsDefaults(catalog commonprovider.Catalog, defaults contract.ExecutionDefaults) error {
	registration, err := catalog.Lookup(defaults.Provider)
	if err != nil {
		return fmt.Errorf("%w: unsupported Member provider: %w", contract.ErrInvalidDefinition, err)
	}
	if !registration.Description.Discoverable {
		return fmt.Errorf("%w: Member provider has no supported execution profile", contract.ErrInvalidDefinition)
	}
	options := registration.Description.SupportedOptions
	if defaults.Model != "" && !slices.Contains(options, commonprovider.OptionModel) {
		return fmt.Errorf("%w: provider does not support model overrides", contract.ErrInvalidDefinition)
	}
	if defaults.Effort != "" && defaults.Effort != "default" && !slices.Contains(options, commonprovider.OptionEffort) {
		return fmt.Errorf("%w: provider does not support effort overrides", contract.ErrInvalidDefinition)
	}
	return nil
}
