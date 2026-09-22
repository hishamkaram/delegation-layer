package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const modelDiscoverySource = "opencode models --verbose"

// ModelDiscovery describes OpenCode's verbose model listing. Each model ID is
// followed by one JSON metadata object; only its display name and variants
// become model facts.
func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments:  []string{"models", "--verbose"},
		Source:     modelDiscoverySource,
		Capability: commonprovider.RuntimeCapability{HelpArgs: []string{"models"}, RequiredFlags: []string{"--verbose"}},
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
	models, err := parseModelListing(stdout)
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
		return finishCatalog(baseCatalog("unavailable", "models_unavailable", efforts, effortsListed))
	}
	catalog := baseCatalog("available", "ok", efforts, effortsListed)
	catalog.Complete = true
	catalog.Models = models
	return finishCatalog(catalog)
}

func parseModelListing(data []byte) ([]commonprovider.ModelInfo, error) {
	if len(data) > int(commonprovider.MaxInspectionOutput) {
		return nil, errors.New("opencode model listing exceeds output bound")
	}
	reader := bufio.NewReader(bytes.NewReader(data))
	models := make([]commonprovider.ModelInfo, 0)
	seen := make(map[string]struct{})
	for {
		id, err := nextModelID(reader)
		if errors.Is(err, io.EOF) {
			return models, nil
		}
		if err != nil {
			return models, err
		}
		var raw json.RawMessage
		decoder := json.NewDecoder(reader)
		if decodeErr := decoder.Decode(&raw); decodeErr != nil {
			return models, fmt.Errorf("opencode metadata for %q is malformed: %w", id, decodeErr)
		}
		buffered := decoder.Buffered()
		reader = bufio.NewReader(io.MultiReader(buffered, reader))
		if structureErr := task.ValidateJSONStructure(raw); structureErr != nil {
			return models, fmt.Errorf("opencode metadata for %q is malformed: %w", id, structureErr)
		}
		if _, exists := seen[id]; exists {
			return models, fmt.Errorf("duplicate opencode model id %q", id)
		}
		model, err := decodeModelMetadata(id, raw)
		if err != nil {
			return models, fmt.Errorf("opencode model %q: %w", id, err)
		}
		seen[id] = struct{}{}
		models = append(models, model)
		if len(models) > 4096 {
			return models, errors.New("opencode model listing exceeds model bound")
		}
	}
}

func nextModelID(reader *bufio.Reader) (string, error) {
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			if strings.HasPrefix(line, "{") || len(strings.Fields(line)) != 1 {
				return "", errors.New("opencode listing expected one model id before metadata")
			}
			return line, nil
		}
		if err != nil {
			return "", err
		}
	}
}

type verboseModelMetadata struct {
	Name           string          `json:"name"`
	DisplayName    string          `json:"displayName"`
	Variants       json.RawMessage `json:"variants"`
	DefaultVariant string          `json:"defaultVariant"`
	DefaultEffort  string          `json:"defaultEffort"`
}

func decodeModelMetadata(id string, data []byte) (commonprovider.ModelInfo, error) {
	var metadata verboseModelMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return commonprovider.ModelInfo{}, errors.New("metadata is not an object")
	}
	model := commonprovider.ModelInfo{ID: id, Name: metadata.Name}
	if model.Name == "" {
		model.Name = metadata.DisplayName
	}
	if len(metadata.Variants) != 0 && !bytes.Equal(bytes.TrimSpace(metadata.Variants), []byte("null")) {
		var variants map[string]json.RawMessage
		if err := json.Unmarshal(metadata.Variants, &variants); err != nil || variants == nil {
			return commonprovider.ModelInfo{}, errors.New("variants is not an object")
		}
		model.Efforts = make([]string, 0, len(variants))
		for key := range variants {
			model.Efforts = append(model.Efforts, key)
		}
		sort.Strings(model.Efforts)
	}
	model.DefaultEffort = metadata.DefaultVariant
	if model.DefaultEffort == "" {
		model.DefaultEffort = metadata.DefaultEffort
	}
	return model, nil
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
