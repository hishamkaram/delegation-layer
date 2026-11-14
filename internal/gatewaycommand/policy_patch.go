package gatewaycommand

import (
	"errors"
	"slices"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

// patchProviderPolicies changes only explicitly requested fields. The SDK's
// copied profile/workspace policy is the base, never a second CLI default set.
func patchProviderPolicies(a Arguments, base []deviceclient.ProviderPolicy, descriptions []provider.Description) ([]deviceclient.ProviderPolicy, error) {
	selected := slices.Clone(base)
	if a.Provider != "" {
		indexDescription := slices.IndexFunc(descriptions, func(d provider.Description) bool { return d.ID == a.Provider && d.Discoverable })
		if indexDescription < 0 {
			return nil, errors.New("requested provider is not in the compiled Native catalog")
		}
		if err := validatePolicyPatch(a, descriptions[indexDescription]); err != nil {
			return nil, err
		}
		index := slices.IndexFunc(base, func(p deviceclient.ProviderPolicy) bool { return p.Provider == a.Provider })
		if index >= 0 {
			selected = []deviceclient.ProviderPolicy{base[index]}
		} else {
			var err error
			initial := a
			if !a.permissionSet {
				initial.Permission = config.ModeReadOnly
			}
			if !a.budgetSet {
				initial.MaximumBudget = defaultMaximumBudget
			}
			selected, err = providerPolicies(initial, descriptions)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("policy change requires an existing policy or explicit --provider")
	}
	for i := range selected {
		p, err := patchProviderPolicy(a, selected[i])
		if err != nil {
			return nil, err
		}
		selected[i] = p
	}
	return selected, nil
}

func patchProviderPolicy(a Arguments, p deviceclient.ProviderPolicy) (deviceclient.ProviderPolicy, error) {
	p.Models, p.Efforts, p.Permissions = slices.Clone(p.Models), slices.Clone(p.Efforts), slices.Clone(p.Permissions)
	if a.permissionSet {
		if err := config.ValidateMode(a.Permission); err != nil {
			return p, err
		}
		p.Permissions = []string{a.Permission}
	}
	if a.budgetSet {
		budget, err := maximumBudgetSeconds(a.MaximumBudget)
		if err != nil {
			return p, err
		}
		p.MaxBudgetSeconds = budget
	}
	if a.modelSet {
		p.NativeModel = a.Model == ""
		p.Models = nil
		if a.Model != "" {
			p.Models = []string{a.Model}
		}
	}
	if a.effortSet {
		p.NativeEffort = a.Effort == ""
		p.Efforts = nil
		if a.Effort != "" {
			p.Efforts = []string{a.Effort}
		}
	}
	return p, nil
}

func validatePolicyPatch(a Arguments, d provider.Description) error {
	if a.permissionSet && !slices.Contains(d.SupportedModes, a.Permission) {
		return errors.New("provider does not support the requested permission")
	}
	if err := validateSelectionPatch(a.modelSet, a.Model, provider.OptionModel, d); err != nil {
		return err
	}
	return validateSelectionPatch(a.effortSet, a.Effort, provider.OptionEffort, d)
}

func validateSelectionPatch(selected bool, value, option string, d provider.Description) error {
	if !selected {
		return nil
	}
	if value == "" && slices.Contains(d.RequiredOptions, option) {
		return errors.New("provider requires an explicit selection")
	}
	if value != "" && !slices.Contains(d.SupportedOptions, option) {
		return errors.New("provider does not support the requested selection")
	}
	return nil
}
