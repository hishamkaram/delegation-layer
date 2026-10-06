package ccr

import (
	"errors"
	"fmt"
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

const modelSource = "ccr model list"

func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments: []string{"model", "list"}, Source: modelSource,
		Capability: commonprovider.RuntimeCapability{VersionArgs: []string{"version"}, HelpArgs: []string{"model", "list"}},
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
	models, err := parseModelList(stdout)
	if err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	catalog.Models = append(catalog.Models, models...)
	if len(catalog.Models) > 0 {
		catalog.Status, catalog.ReasonCode, catalog.Complete = "available", "ok", true
	}
	if err := commonprovider.ValidateModelCatalog(catalog); err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	return catalog, nil
}

func parseModelList(stdout []byte) ([]commonprovider.ModelInfo, error) {
	if len(stdout) > int(commonprovider.MaxInspectionOutput) {
		return nil, errors.New("CCR model listing exceeds inspection limit")
	}
	if strings.TrimSpace(string(stdout)) == "No model aliases configured." {
		return nil, nil
	}
	models := make([]commonprovider.ModelInfo, 0)
	for _, rawLine := range strings.Split(string(stdout), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 4 || strings.TrimSpace(fields[0]) == "" {
			return nil, fmt.Errorf("invalid CCR model row")
		}
		values := make(map[string]string, len(fields)-1)
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("invalid CCR model row")
			}
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
		provider, providerOK := values["provider"]
		providerModel, providerModelOK := values["model"]
		compatibility, compatibilityOK := values["compat"]
		if !providerOK || !providerModelOK || !compatibilityOK {
			return nil, fmt.Errorf("invalid CCR model row")
		}
		alias := strings.TrimSpace(fields[0])
		models = append(models, commonprovider.ModelInfo{
			ID:   alias,
			Name: fmt.Sprintf("%s (%s/%s; %s)", alias, provider, providerModel, compatibility),
		})
	}
	if len(models) == 0 {
		return nil, errors.New("invalid CCR model listing")
	}
	return models, nil
}
