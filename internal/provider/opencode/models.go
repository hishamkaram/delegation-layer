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

const modelDiscoverySource = "opencode models --standalone"

// ModelDiscovery describes OpenCode's model listing. The listing is one model
// ID per line; an ID may be followed by one JSON metadata object, and only its
// display name and variants become model facts. Without metadata, a model's
// efforts stay unknown. Discovery runs a private standalone server so it never
// starts or uses the shared user background service.
func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments:  []string{"models", "--standalone"},
		Source:     modelDiscoverySource,
		Capability: commonprovider.RuntimeCapability{HelpArgs: []string{"models"}, RequiredFlags: []string{"--standalone"}},
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
		if _, exists := seen[id]; exists {
			return models, fmt.Errorf("duplicate opencode model id %q", id)
		}
		model := commonprovider.ModelInfo{ID: id}
		hasMetadata, err := metadataFollows(reader)
		if err != nil {
			return models, fmt.Errorf("opencode listing for %q is unreadable: %w", id, err)
		}
		if hasMetadata {
			if model, reader, err = readModelMetadata(id, reader); err != nil {
				return models, err
			}
		}
		seen[id] = struct{}{}
		models = append(models, model)
		if len(models) > 4096 {
			return models, errors.New("opencode model listing exceeds model bound")
		}
	}
}

// metadataFollows skips whitespace and reports whether the next token opens a
// JSON metadata object for the model ID just read.
func metadataFollows(reader *bufio.Reader) (bool, error) {
	for {
		next, err := reader.Peek(1)
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !strings.ContainsRune(" \t\r\n", rune(next[0])) {
			return next[0] == '{', nil
		}
		if _, err = reader.Discard(1); err != nil {
			return false, err
		}
	}
}

// readModelMetadata decodes one metadata object and returns the reader
// positioned after it.
func readModelMetadata(id string, reader *bufio.Reader) (commonprovider.ModelInfo, *bufio.Reader, error) {
	var raw json.RawMessage
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&raw); err != nil {
		return commonprovider.ModelInfo{}, reader, fmt.Errorf("opencode metadata for %q is malformed: %w", id, err)
	}
	rest := bufio.NewReader(io.MultiReader(decoder.Buffered(), reader))
	if err := task.ValidateJSONStructure(raw); err != nil {
		return commonprovider.ModelInfo{}, rest, fmt.Errorf("opencode metadata for %q is malformed: %w", id, err)
	}
	model, err := decodeModelMetadata(id, raw)
	if err != nil {
		return commonprovider.ModelInfo{}, rest, fmt.Errorf("opencode model %q: %w", id, err)
	}
	return model, rest, nil
}

func nextModelID(reader *bufio.Reader) (string, error) {
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			if strings.HasPrefix(line, "{") || len(strings.Fields(line)) != 1 {
				return "", errors.New("opencode listing expected one model id per line")
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
