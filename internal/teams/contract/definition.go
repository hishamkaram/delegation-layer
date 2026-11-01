// Package contract owns Team definitions and pure validity, not runtime readiness.
package contract

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrInvalidDefinition = errors.New("invalid team definition")

type Role struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Responsibility string `json:"responsibility"`
}

type ExecutionDefaults struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

type Member struct {
	ID                string             `json:"id"`
	Name              string             `json:"name"`
	RoleIDs           []string           `json:"role_ids"`
	ExecutionDefaults *ExecutionDefaults `json:"execution_defaults,omitempty"`
}

type Team struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Purpose       string   `json:"purpose"`
	Roles         []Role   `json:"roles"`
	Members       []Member `json:"members"`
}

type Record struct {
	Reference  task.TeamReference `json:"reference"`
	Revision   int64              `json:"revision"`
	Definition Team               `json:"definition"`
}

type ResolvedMember struct {
	CatalogID string
	Record    Record
	Member    Member
}

func Validate(team Team) error {
	if team.SchemaVersion != 1 || team.Roles == nil || team.Members == nil {
		return invalid("schema_version/roles/members", errors.New("version 1 and explicit arrays are required"))
	}
	if err := task.ValidateTeamID(team.ID); err != nil {
		return invalid("id", err)
	}
	if !validText(team.Name) || !validText(team.Purpose) {
		return invalid("name/purpose", errors.New("nonblank UTF-8 text required"))
	}
	roles := make(map[string]bool, len(team.Roles))
	for _, role := range team.Roles {
		if err := validateRole(role, roles); err != nil {
			return err
		}
		roles[role.ID] = true
	}
	members := make(map[string]bool, len(team.Members))
	for _, member := range team.Members {
		if err := validateMember(member, roles, members); err != nil {
			return err
		}
		members[member.ID] = true
	}
	return nil
}

func validateRole(role Role, seen map[string]bool) error {
	if err := task.ValidateTeamID(role.ID); err != nil {
		return invalid("roles.id", err)
	}
	if seen[role.ID] || !validText(role.Name) || !validText(role.Responsibility) {
		return invalid("roles", errors.New("duplicate ID or missing name/responsibility"))
	}
	return nil
}

func validateMember(member Member, roles, seen map[string]bool) error {
	if err := task.ValidateTeamID(member.ID); err != nil {
		return invalid("members.id", err)
	}
	if seen[member.ID] || !validText(member.Name) || len(member.RoleIDs) == 0 {
		return invalid("members", errors.New("duplicate ID, missing name or empty role references"))
	}
	memberRoles := make(map[string]bool, len(member.RoleIDs))
	for _, id := range member.RoleIDs {
		if !roles[id] || memberRoles[id] {
			return invalid("members.role_ids", errors.New("unknown or repeated role reference"))
		}
		memberRoles[id] = true
	}
	if member.ExecutionDefaults != nil {
		return validateDefaults(*member.ExecutionDefaults)
	}
	return nil
}

func validateDefaults(defaults ExecutionDefaults) error {
	if err := policy.ValidateProvider(defaults.Provider); err != nil {
		return invalid("members.execution_defaults.provider", err)
	}
	if !utf8.ValidString(defaults.Model) || !utf8.ValidString(defaults.Effort) {
		return invalid("members.execution_defaults", errors.New("invalid UTF-8 option"))
	}
	if err := task.ValidateRequestedOptions(task.TaskConfig{Model: defaults.Model, Effort: defaults.Effort}); err != nil {
		return invalid("members.execution_defaults", err)
	}
	return nil
}

func validText(value string) bool { return utf8.ValidString(value) && strings.TrimSpace(value) != "" }

func invalid(field string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrInvalidDefinition, field, err)
}

func Decode(data []byte) (Team, error) {
	if err := validateDefinitionEncoding(data); err != nil {
		return Team{}, invalid("definition", err)
	}
	var team Team
	if err := task.DecodeStrict(data, &team); err != nil {
		return Team{}, invalid("definition", err)
	}
	if err := Validate(team); err != nil {
		return Team{}, err
	}
	return team, nil
}

func Encode(team Team) ([]byte, error) {
	if err := Validate(team); err != nil {
		return nil, err
	}
	data, err := task.MarshalCanonical(team)
	if err != nil {
		return nil, invalid("definition", err)
	}
	if len(data) > task.MaxControlRecordSize {
		return nil, invalid("definition", task.ErrControlRecordTooBig)
	}
	return data, nil
}

func (team Team) Clone() Team {
	if team.Roles != nil {
		team.Roles = append([]Role{}, team.Roles...)
	}
	if team.Members == nil {
		return team
	}
	members := make([]Member, len(team.Members))
	for i, member := range team.Members {
		member.RoleIDs = append([]string{}, member.RoleIDs...)
		if member.ExecutionDefaults != nil {
			defaults := *member.ExecutionDefaults
			member.ExecutionDefaults = &defaults
		}
		members[i] = member
	}
	team.Members = members
	return team
}
