package ccr

import (
	"encoding/json"
	"errors"
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

const modelSource = "ccr model list --json"

func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments: []string{"model", "list", "--json"}, Source: modelSource,
		Capability: commonprovider.RuntimeCapability{HelpArgs: []string{"model", "list"}, RequiredFlags: []string{"--json"}},
		Project:    projectModels,
	}
}

func projectModels(stdout, stderr, _ []byte, success bool) (commonprovider.ModelCatalog, error) {
	catalog := commonprovider.ModelCatalog{Status: "unavailable", ReasonCode: "models_unavailable", Source: modelSource, Models: []commonprovider.ModelInfo{}}
	if !success {
		catalog.Status, catalog.ReasonCode = "failed", "native_command_failed"
		if commonprovider.ModelInterfaceUnavailable(stdout, stderr) {
			catalog.Status, catalog.ReasonCode = "unavailable", "interface_unavailable"
		}
		if commonprovider.ModelAuthFailure(stdout, stderr) {
			catalog.Status, catalog.ReasonCode = "blocked", "authentication_unavailable"
		}
		return catalog, nil
	}
	var document struct {
		SchemaVersion int `json:"schema_version"`
		Models        []struct {
			Alias         string `json:"alias"`
			Provider      string `json:"provider"`
			ProviderModel string `json:"provider_model"`
			Compatibility string `json:"compatibility"`
		} `json:"models"`
	}
	if len(stdout) > int(commonprovider.MaxInspectionOutput) || json.Unmarshal(stdout, &document) != nil || document.SchemaVersion != 1 || document.Models == nil {
		return commonprovider.ModelCatalog{}, errors.New("invalid CCR model listing")
	}
	for _, model := range document.Models {
		catalog.Models = append(catalog.Models, commonprovider.ModelInfo{ID: model.Alias, Name: fmt.Sprintf("%s (%s/%s; %s)", model.Alias, model.Provider, model.ProviderModel, model.Compatibility)})
	}
	if len(catalog.Models) > 0 {
		catalog.Status, catalog.ReasonCode, catalog.Complete = "available", "ok", true
	}
	if err := commonprovider.ValidateModelCatalog(catalog); err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	return catalog, nil
}
