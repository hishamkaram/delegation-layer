package contract

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type Execution struct {
	Provider string
	Auto     bool
	Config   task.TaskConfig
}

// Selection is the complete value shown for an explicit Member selection.
// Catalog reads own its inputs; neither this value nor its equality grants
// admission authority or changes persisted Team definitions.
type Selection struct {
	Execution Execution
	Context   *task.TeamContext
}

var ErrTeamSelectionChanged = fmt.Errorf("%w: Team selection changed; preview it again before dispatch", task.ErrInvalidTeamSelection)

func Select(member ResolvedMember, intent task.TeamIntent) (Selection, error) {
	execution, err := MergeExecution(member.Member.ExecutionDefaults, intent)
	if err != nil {
		return Selection{}, err
	}
	selection, err := SelectionContext(member, intent)
	if err != nil {
		return Selection{}, err
	}
	return Selection{Execution: execution, Context: selection}, nil
}

func (s Selection) Equal(other Selection) bool { return reflect.DeepEqual(s, other) }

func (s Selection) Clone() Selection {
	s.Context = s.Context.Clone()
	if s.Execution.Config.ReviewTarget != nil {
		target := *s.Execution.Config.ReviewTarget
		s.Execution.Config.ReviewTarget = &target
	}
	return s
}

// MergeExecution preserves caller choices; defaults never grant task permissions.
func MergeExecution(defaults *ExecutionDefaults, requested task.TeamIntent) (Execution, error) {
	result := Execution{Provider: requested.Provider, Auto: requested.Auto, Config: requested.Config}
	if err := task.ValidateRequestedOptions(requested.Config); err != nil {
		return Execution{}, err
	}
	if requested.Auto {
		if requested.Provider != "" || defaults != nil {
			return Execution{}, errors.New("auto cannot be combined with an explicit or Member-bound provider")
		}
		return result, nil
	}
	if defaults != nil && (requested.Provider == "" || requested.Provider == defaults.Provider) {
		result.Provider = defaults.Provider
		if result.Config.Model == "" {
			result.Config.Model = defaults.Model
		}
		if result.Config.Effort == "" {
			result.Config.Effort = defaults.Effort
		}
	}
	if result.Provider == "" {
		return Execution{}, errors.New("Member without execution defaults requires an explicit provider or auto")
	}
	return result, nil
}

func SelectionContext(member ResolvedMember, intent task.TeamIntent) (*task.TeamContext, error) {
	if err := validateResolvedMember(member); err != nil {
		return nil, fmt.Errorf("%w: %w", task.ErrInvalidTeamSelection, err)
	}
	roles := make([]task.TeamRoleContext, 0, len(member.Member.RoleIDs))
	for _, id := range member.Member.RoleIDs {
		for _, role := range member.Record.Definition.Roles {
			if role.ID == id {
				roles = append(roles, task.TeamRoleContext{ID: role.ID, Name: role.Name, Responsibility: role.Responsibility})
			}
		}
	}
	selection := &task.TeamContext{
		SchemaVersion: 1,
		Target:        task.TeamTarget{Team: member.Record.Reference.Clone(), MemberID: member.Member.ID, CatalogID: member.CatalogID},
		TeamRevision:  member.Record.Revision, TeamName: member.Record.Definition.Name,
		TeamPurpose: member.Record.Definition.Purpose, MemberName: member.Member.Name, Roles: roles, Intent: intent,
	}
	if err := task.ValidateTeamContext(selection); err != nil {
		return nil, fmt.Errorf("creating selection context: %w", err)
	}
	return selection.Clone(), nil
}

func validateResolvedMember(resolved ResolvedMember) error {
	if err := Validate(resolved.Record.Definition); err != nil {
		return err
	}
	if resolved.Record.Reference.TeamID != resolved.Record.Definition.ID {
		return invalid("resolved_member", errors.New("Team identity does not match definition"))
	}
	for _, member := range resolved.Record.Definition.Members {
		if member.ID == resolved.Member.ID && reflect.DeepEqual(member, resolved.Member) {
			return nil
		}
	}
	return invalid("resolved_member", errors.New("Member does not match its authoritative definition"))
}
