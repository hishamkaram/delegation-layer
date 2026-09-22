package antigravity

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

const modelDiscoverySource = "agy models"

// ModelDiscovery describes agy's observed tab-separated model listing. The
// first field is the exact native model slug; agy may encode effort variants
// in that slug and they must remain unchanged.
func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments:  []string{"models"},
		Source:     modelDiscoverySource,
		Capability: commonprovider.RuntimeCapability{HelpArgs: []string{"models"}},
		Project:    projectModels,
	}
}

func projectModels(stdout, stderr, help []byte, success bool) (commonprovider.ModelCatalog, error) {
	efforts, effortsListed := commonprovider.ParseListedModelEfforts(help, "--effort")
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
	models, err := parseModelsTSV(stdout)
	if err != nil {
		if len(models) == 0 && commonprovider.ModelAuthFailure(stdout, stderr) {
			return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, effortsListed))
		}
		return commonprovider.ModelCatalog{}, err
	}
	if len(models) == 0 {
		if commonprovider.ModelAuthFailure(stdout, stderr) {
			return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, effortsListed))
		}
		return finishCatalog(baseCatalog("unavailable", "models_unavailable", efforts, effortsListed))
	}
	catalog := baseCatalog("available", "ok", efforts, effortsListed)
	catalog.Complete = true
	catalog.Models = models
	return finishCatalog(catalog)
}

func parseModelsTSV(data []byte) ([]commonprovider.ModelInfo, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), int(commonprovider.MaxInspectionOutput))
	models := make([]commonprovider.ModelInfo, 0)
	seen := make(map[string]struct{})
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" || line == "Fetching available models..." {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			return models, fmt.Errorf("agy models line %d is not id<TAB>name", lineNumber)
		}
		id := strings.TrimSpace(fields[0])
		if id == "" {
			return models, fmt.Errorf("agy models line %d has an empty id", lineNumber)
		}
		if _, exists := seen[id]; exists {
			return models, fmt.Errorf("agy models line %d repeats id %q", lineNumber, id)
		}
		seen[id] = struct{}{}
		models = append(models, commonprovider.ModelInfo{ID: id, Name: strings.TrimSpace(fields[1])})
		if len(models) > 4096 {
			return models, errors.New("agy models listing exceeds model bound")
		}
	}
	if err := scanner.Err(); err != nil {
		return models, fmt.Errorf("read agy models listing: %w", err)
	}
	return models, nil
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
