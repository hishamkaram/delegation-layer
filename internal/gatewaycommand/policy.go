package gatewaycommand

import (
	"errors"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

func providerPolicies(a Arguments, descriptions []provider.Description) ([]deviceclient.ProviderPolicy, error) {
	if _, err := maximumBudgetSeconds(a.MaximumBudget); err != nil {
		return nil, err
	}
	if err := config.ValidateMode(a.Permission); err != nil {
		return nil, err
	}
	policies := make([]deviceclient.ProviderPolicy, 0, len(descriptions))
	for _, description := range descriptions {
		if a.Provider != "" && a.Provider != description.ID {
			continue
		}
		policy, err := descriptionPolicy(a, description)
		if err != nil {
			if a.Provider != "" {
				return nil, err
			}
			continue
		}
		policies = append(policies, policy)
	}
	if len(policies) == 0 {
		return nil, errors.New("no compiled provider matches the requested gateway policy")
	}
	return policies, nil
}

func descriptionPolicy(a Arguments, d provider.Description) (deviceclient.ProviderPolicy, error) {
	if !d.Discoverable || !slices.Contains(d.SupportedModes, a.Permission) {
		return deviceclient.ProviderPolicy{}, errors.New("provider does not support the requested gateway permission")
	}
	for _, required := range d.RequiredOptions {
		if (required == provider.OptionModel && a.Model != "") || (required == provider.OptionEffort && a.Effort != "") {
			continue
		}
		return deviceclient.ProviderPolicy{}, errors.New("provider requires an unspecified execution option")
	}
	if a.Model != "" && !slices.Contains(d.SupportedOptions, provider.OptionModel) {
		return deviceclient.ProviderPolicy{}, errors.New("provider does not accept model selection")
	}
	if a.Effort != "" && !slices.Contains(d.SupportedOptions, provider.OptionEffort) {
		return deviceclient.ProviderPolicy{}, errors.New("provider does not accept effort selection")
	}
	budget, err := maximumBudgetSeconds(a.MaximumBudget)
	if err != nil {
		return deviceclient.ProviderPolicy{}, err
	}
	p := deviceclient.ProviderPolicy{Provider: d.ID, Permissions: []string{a.Permission}, MaxBudgetSeconds: budget, NativeModel: a.Model == "", NativeEffort: a.Effort == ""}
	if a.Model != "" {
		p.Models = []string{a.Model}
	}
	if a.Effort != "" {
		p.Efforts = []string{a.Effort}
	}
	return p, nil
}

func maximumBudgetSeconds(budget time.Duration) (uint32, error) {
	if budget <= 0 || budget%time.Second != 0 || budget/time.Second > 1<<32-1 {
		return 0, errors.New("gateway max-budget requires positive whole seconds within protocol range")
	}
	return uint32(budget / time.Second), nil
}
