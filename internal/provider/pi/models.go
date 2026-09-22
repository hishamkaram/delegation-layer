package pi

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

const modelDiscoverySource = "pi --list-models"

// ModelDiscovery describes Pi's fixed six-column model table. Pi emits no
// protocol exchange for this command, so the shared runner closes stdin.
func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments:  []string{"--list-models"},
		Source:     modelDiscoverySource,
		Capability: commonprovider.RuntimeCapability{RequiredFlags: []string{"--list-models"}},
		Project:    projectModels,
	}
}

func projectModels(stdout, stderr, help []byte, success bool) (commonprovider.ModelCatalog, error) {
	efforts, effortsListed := commonprovider.ParseListedModelEfforts(help, "--thinking")
	if !success {
		if commonprovider.ModelAuthFailure(stdout, stderr) {
			return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, effortsListed))
		}
		status, reason := "failed", "native_command_failed"
		if commonprovider.ModelInterfaceUnavailable(stdout, stderr) {
			status, reason = "unavailable", "interface_unavailable"
		}
		return finishCatalog(baseCatalog(status, reason, efforts, effortsListed))
	}
	models, recognized, err := parseModelTable(stdout)
	if err != nil {
		if commonprovider.ModelAuthFailure(stdout, stderr) {
			return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, effortsListed))
		}
		return commonprovider.ModelCatalog{}, err
	}
	if len(models) == 0 {
		if commonprovider.ModelAuthFailure(stdout, stderr) {
			return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, effortsListed))
		}
		reason := "models_unavailable"
		if !recognized {
			reason = "interface_unavailable"
		}
		return finishCatalog(baseCatalog("unavailable", reason, efforts, effortsListed))
	}
	catalog := baseCatalog("available", "ok", efforts, effortsListed)
	catalog.Complete = true
	catalog.Models = models
	return finishCatalog(catalog)
}

func parseModelTable(data []byte) ([]commonprovider.ModelInfo, bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), int(commonprovider.MaxInspectionOutput))
	models := make([]commonprovider.ModelInfo, 0)
	seen := make(map[string]struct{})
	headerSeen := false
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if !headerSeen {
			if strings.Join(fields, " ") != "provider model context max-out thinking images" {
				return models, false, fmt.Errorf("pi model listing line %d is not the known table header", lineNumber)
			}
			headerSeen = true
			continue
		}
		if len(fields) != 6 {
			return models, true, fmt.Errorf("pi model listing line %d does not have six columns", lineNumber)
		}
		provider, modelID := fields[0], fields[1]
		if provider == "" || modelID == "" {
			return models, true, fmt.Errorf("pi model listing line %d has an empty provider or model", lineNumber)
		}
		id := provider + "/" + modelID
		if _, exists := seen[id]; exists {
			return models, true, fmt.Errorf("pi model listing line %d repeats id %q", lineNumber, id)
		}
		seen[id] = struct{}{}
		models = append(models, commonprovider.ModelInfo{ID: id})
		if len(models) > 4096 {
			return models, true, errors.New("pi model listing exceeds model bound")
		}
	}
	if err := scanner.Err(); err != nil {
		return models, headerSeen, fmt.Errorf("read Pi model listing: %w", err)
	}
	return models, headerSeen, nil
}

func baseCatalog(status, reason string, efforts []string, listed bool) commonprovider.ModelCatalog {
	catalog := commonprovider.ModelCatalog{
		Status:     status,
		ReasonCode: reason,
		Source:     modelDiscoverySource,
		Models:     []commonprovider.ModelInfo{},
	}
	if listed {
		catalog.Efforts = efforts
	}
	return catalog
}

func finishCatalog(catalog commonprovider.ModelCatalog) (commonprovider.ModelCatalog, error) {
	if err := commonprovider.ValidateModelCatalog(catalog); err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	return catalog, nil
}
