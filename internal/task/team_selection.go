package task

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

var ErrInvalidTeamSelection = errors.New("invalid team selection")

var teamIdentifier = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// TeamProject identifies a location, not an account or a permission boundary.
type TeamProject struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type TeamReference struct {
	Scope   string       `json:"scope"`
	TeamID  string       `json:"team_id"`
	Project *TeamProject `json:"project,omitempty"`
}

// TeamTarget is caller intent. CatalogID is pinned by schedule creation, not argv.
type TeamTarget struct {
	Team      TeamReference `json:"team"`
	MemberID  string        `json:"member_id"`
	CatalogID string        `json:"catalog_id,omitempty"`
}

// TeamIntent records explicit options before Member/provider defaults were applied.
// Execution always uses TaskRecord's canonical fields, never this evidence.
type TeamIntent struct {
	Provider string     `json:"provider,omitempty"`
	Auto     bool       `json:"auto,omitempty"`
	Config   TaskConfig `json:"config"`
}

type TeamRoleContext struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Responsibility string `json:"responsibility"`
}

type TeamContext struct {
	SchemaVersion int               `json:"schema_version"`
	Target        TeamTarget        `json:"target"`
	TeamRevision  int64             `json:"team_revision"`
	TeamName      string            `json:"team_name"`
	TeamPurpose   string            `json:"team_purpose"`
	MemberName    string            `json:"member_name"`
	Roles         []TeamRoleContext `json:"roles"`
	Intent        TeamIntent        `json:"intent"`
}

func ValidateTeamID(id string) error {
	if len(id) > 64 || !teamIdentifier.MatchString(id) {
		return fmt.Errorf("%w: identifier must be 1-64 lowercase letters/digits with internal single hyphens", ErrInvalidTeamSelection)
	}
	return nil
}

func ValidateTeamReference(reference TeamReference) error {
	if err := ValidateTeamID(reference.TeamID); err != nil {
		return err
	}
	switch reference.Scope {
	case "user":
		if reference.Project != nil {
			return fmt.Errorf("%w: user scope cannot carry a project", ErrInvalidTeamSelection)
		}
	case "project":
		if reference.Project == nil || !IsCanonicalCwd(reference.Project.Path) ||
			(reference.Project.Kind != "git" && reference.Project.Kind != "directory") {
			return fmt.Errorf("%w: project scope requires a canonical git/directory project", ErrInvalidTeamSelection)
		}
	default:
		return fmt.Errorf("%w: scope must be user or project", ErrInvalidTeamSelection)
	}
	return nil
}

func ValidateTeamTarget(target TeamTarget, requireCatalog bool) error {
	if err := ValidateTeamReference(target.Team); err != nil {
		return err
	}
	if err := ValidateTeamID(target.MemberID); err != nil {
		return err
	}
	if target.CatalogID != "" || requireCatalog {
		if err := ValidateRootID(target.CatalogID); err != nil {
			return fmt.Errorf("%w: invalid catalog identity: %w", ErrInvalidTeamSelection, err)
		}
	}
	return nil
}

func ValidateTeamContext(context *TeamContext) error {
	if context == nil {
		return nil
	}
	if context.SchemaVersion != 1 || context.TeamRevision < 1 || len(context.Roles) == 0 {
		return fmt.Errorf("%w: invalid version, revision or empty roles", ErrInvalidTeamSelection)
	}
	if err := ValidateTeamTarget(context.Target, true); err != nil {
		return err
	}
	for _, value := range []string{context.TeamName, context.TeamPurpose, context.MemberName} {
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: invalid descriptive text", ErrInvalidTeamSelection)
		}
	}
	if err := validateTeamRoles(context.Roles); err != nil {
		return err
	}
	if context.Intent.Auto && context.Intent.Provider != "" {
		return fmt.Errorf("%w: provider and auto are mutually exclusive", ErrInvalidTeamSelection)
	}
	if context.Intent.Provider != "" {
		if err := policy.ValidateProvider(context.Intent.Provider); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidTeamSelection, err)
		}
	}
	return ValidateRequestedOptions(context.Intent.Config)
}

func validateTeamRoles(roles []TeamRoleContext) error {
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		if err := ValidateTeamID(role.ID); err != nil {
			return err
		}
		if seen[role.ID] || !utf8.ValidString(role.Name) || strings.TrimSpace(role.Name) == "" ||
			!utf8.ValidString(role.Responsibility) || strings.TrimSpace(role.Responsibility) == "" {
			return fmt.Errorf("%w: duplicate or invalid role", ErrInvalidTeamSelection)
		}
		seen[role.ID] = true
	}
	return nil
}

func (reference TeamReference) Clone() TeamReference {
	if reference.Project != nil {
		project := *reference.Project
		reference.Project = &project
	}
	return reference
}

func (target TeamTarget) Clone() TeamTarget {
	target.Team = target.Team.Clone()
	return target
}

func (context *TeamContext) Clone() *TeamContext {
	if context == nil {
		return nil
	}
	copy := *context
	copy.Target = context.Target.Clone()
	copy.Roles = append([]TeamRoleContext(nil), context.Roles...)
	if context.Intent.Config.ReviewTarget != nil {
		target := *context.Intent.Config.ReviewTarget
		copy.Intent.Config.ReviewTarget = &target
	}
	return &copy
}
