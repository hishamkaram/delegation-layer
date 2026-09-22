package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ModelsRevision identifies the provider model-discovery contract. It is
// independent of any provider release and changes when the discovery
// definition or its facts change.
const ModelsRevision = "model-discovery-v1"

// MaxModelFactsBytes is the journal bound for one sanitized model catalog.
const MaxModelFactsBytes = 512 << 10

const maxModelFactsRows = 4096

// ModelInfo is one provider-reported model. A nil Efforts slice means that
// the provider did not report per-model effort metadata; an empty, non-nil
// slice means that it explicitly reported no effort choices.
type ModelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort,omitempty"`
}

// ModelCatalog is the sanitized result of one native model discovery.
// Models and Efforts contain model facts only; provider account, credential,
// header, and option data never crosses this boundary.
type ModelCatalog struct {
	Status     string      `json:"status"`
	ReasonCode string      `json:"reason_code"`
	Complete   bool        `json:"complete"`
	Source     string      `json:"source"`
	Models     []ModelInfo `json:"models"`
	Efforts    []string    `json:"efforts"`
}

// ModelExchange is the pure line protocol used when a provider's discovery
// command needs an initial request and subsequent replies. The shared core
// owns the process, pipes, deadlines, capture, and stdin close.
type ModelExchange interface {
	Start() ([][]byte, error)
	Accept(line []byte) (replies [][]byte, done bool, err error)
}

// ModelDiscoveryDefinition describes one provider-owned discovery command.
// Capability is the command-specific version/help contract checked before
// Arguments runs. Callback fields are intentionally excluded from the
// serialized inspection binding; the core invokes them inside the already-
// supervised lifetime.
type ModelDiscoveryDefinition struct {
	Arguments   []string                                                              `json:"arguments"`
	Source      string                                                                `json:"source"`
	Capability  RuntimeCapability                                                     `json:"capability"`
	NewExchange func() ModelExchange                                                  `json:"-"`
	Project     func(stdout, stderr, help []byte, success bool) (ModelCatalog, error) `json:"-"`
}

// ValidateModelCatalog checks the bounded, nonsecret facts that an adapter is
// allowed to publish. It deliberately validates shape and text safety only;
// model IDs are provider-owned and are not compared with an allowlist.
func ValidateModelCatalog(catalog ModelCatalog) error {
	if err := validateModelCatalogStatus(catalog); err != nil {
		return err
	}
	if len(catalog.Models) > maxModelFactsRows {
		return fmt.Errorf("model catalog has too many models")
	}
	if err := validateModelText("catalog source", catalog.Source, false); err != nil {
		return err
	}
	if err := validateModelText("catalog reason code", catalog.ReasonCode, false); err != nil {
		return err
	}
	if err := validateModelChoices("catalog efforts", catalog.Efforts); err != nil {
		return err
	}
	if err := validateModelRows(catalog.Models); err != nil {
		return err
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("marshal model catalog: %w", err)
	}
	if len(data)+1 > MaxModelFactsBytes {
		return fmt.Errorf("model catalog exceeds %d bytes", MaxModelFactsBytes)
	}
	return nil
}

func validateModelRows(models []ModelInfo) error {
	seen := make(map[string]struct{}, len(models))
	for index, model := range models {
		if err := validateModelText(fmt.Sprintf("model %d id", index), model.ID, false); err != nil {
			return err
		}
		if _, exists := seen[model.ID]; exists {
			return fmt.Errorf("duplicate model id %q", model.ID)
		}
		seen[model.ID] = struct{}{}
		if err := validateModelText(fmt.Sprintf("model %d name", index), model.Name, true); err != nil {
			return err
		}
		if err := validateModelChoices(fmt.Sprintf("model %d efforts", index), model.Efforts); err != nil {
			return err
		}
		if err := validateModelText(fmt.Sprintf("model %d default effort", index), model.DefaultEffort, true); err != nil {
			return err
		}
		if model.DefaultEffort != "" && model.Efforts != nil && !containsString(model.Efforts, model.DefaultEffort) {
			return fmt.Errorf("model %q default effort is not listed", model.ID)
		}
	}
	return nil
}

func validateModelCatalogStatus(catalog ModelCatalog) error {
	if catalog.Source == "" || catalog.ReasonCode == "" {
		return fmt.Errorf("model catalog source and reason code are required")
	}
	if catalog.Models == nil {
		return fmt.Errorf("model catalog models must be an array")
	}
	switch catalog.Status {
	case "available":
		if !catalog.Complete || len(catalog.Models) == 0 {
			return fmt.Errorf("available model catalog must be complete and nonempty")
		}
	case "partial":
		if catalog.Complete || len(catalog.Models) == 0 {
			return fmt.Errorf("partial model catalog must be incomplete and nonempty")
		}
	case "blocked", "unavailable", "failed":
		if catalog.Complete || len(catalog.Models) != 0 {
			return fmt.Errorf("%s model catalog must be incomplete and empty", catalog.Status)
		}
	default:
		return fmt.Errorf("unknown model catalog status %q", catalog.Status)
	}
	return nil
}

func validateModelText(label, value string, optional bool) error {
	if optional && value == "" {
		return nil
	}
	if value == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is invalid UTF-8", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", label)
		}
	}
	return nil
}

func validateModelChoices(label string, choices []string) error {
	if len(choices) > maxModelFactsRows {
		return fmt.Errorf("%s has too many choices", label)
	}
	seen := make(map[string]struct{}, len(choices))
	for _, choice := range choices {
		if err := validateModelText(label, choice, false); err != nil {
			return err
		}
		if _, exists := seen[choice]; exists {
			return fmt.Errorf("%s contains duplicate choice %q", label, choice)
		}
		seen[choice] = struct{}{}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ParseListedModelEfforts extracts effort choices only when help explicitly
// presents a choice list beside the requested flag. It does not infer a
// provider default or an effort vocabulary from prose.
func ParseListedModelEfforts(help []byte, flag string) ([]string, bool) {
	if flag == "" {
		return nil, false
	}
	lines := strings.Split(string(help), "\n")
	for lineIndex, line := range lines {
		lower := strings.ToLower(line)
		index := strings.Index(lower, strings.ToLower(flag))
		if index < 0 {
			continue
		}
		text := line[index+len(flag):]
		text = appendHelpContinuation(lines, lineIndex, text)
		if choices, listed := parseListedEffortText(text); listed {
			return choices, true
		}
	}
	return nil, false
}

func appendHelpContinuation(lines []string, index int, text string) string {
	for next := index + 1; next < len(lines); next++ {
		line := lines[next]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "-") || len(line) == len(strings.TrimLeft(line, " \t")) {
			break
		}
		text += " " + trimmed
	}
	return text
}

func parseListedEffortText(text string) ([]string, bool) {
	choiceText, listed := explicitEffortChoices(text)
	if !listed {
		choiceText, listed = bracketEffortChoices(text)
	}
	if !listed {
		return nil, false
	}
	choiceText = strings.TrimSpace(strings.TrimRight(choiceText, ".)]};"))
	if strings.EqualFold(choiceText, "none") || choiceText == "[]" || choiceText == "{}" {
		return []string{}, true
	}
	parts := strings.FieldsFunc(choiceText, func(r rune) bool {
		return r == ',' || r == '|' || r == ';'
	})
	choices := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && !containsString(choices, part) {
			choices = append(choices, part)
		}
	}
	if len(choices) == 0 {
		return nil, false
	}
	return choices, true
}

func explicitEffortChoices(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, marker := range []string{"choices:", "options:", "values:", "one of:", "level:"} {
		if index := strings.Index(lower, marker); index >= 0 {
			return text[index+len(marker):], true
		}
	}
	return "", false
}

func bracketEffortChoices(text string) (string, bool) {
	for _, bracket := range []struct {
		open, close rune
	}{{'(', ')'}, {'[', ']'}, {'{', '}'}} {
		start := strings.IndexRune(text, bracket.open)
		if start < 0 {
			continue
		}
		end := strings.IndexRune(text[start+1:], bracket.close)
		if end < 0 {
			continue
		}
		candidate := text[start+1 : start+1+end]
		if strings.ContainsAny(candidate, "|,") || strings.EqualFold(strings.TrimSpace(candidate), "none") {
			return candidate, true
		}
	}
	return "", false
}

// ModelAuthFailure recognizes explicit authentication failures while avoiding
// generic permission-denied wording, which may describe a model or workspace
// policy rather than missing credentials.
func ModelAuthFailure(stdout, stderr []byte) bool {
	text := strings.ToLower(string(stdout) + "\n" + string(stderr))
	for _, marker := range []string{
		"authentication required", "authentication failed", "not authenticated",
		"login required", "please log in", "please login", "unauthenticated",
		"invalid api key", "missing api key", "api key required", "no api key",
		"credentials not found", "no credentials", "oauth token expired",
		"token expired", "unauthorized", "401 unauthorized", "403 forbidden",
		"http 401", "http 403", "status code 401", "status code 403",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return modelAuthStatusCode(stdout) || modelAuthStatusCode(stderr)
}

func modelAuthStatusCode(data []byte) bool {
	if modelAuthStatusValue(data) {
		return true
	}
	for _, line := range strings.Split(string(data), "\n") {
		if modelAuthStatusValue([]byte(line)) {
			return true
		}
	}
	return false
}

func modelAuthStatusValue(data []byte) bool {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return false
	}
	return modelAuthStatusNode(value)
}

func modelAuthStatusNode(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		return modelAuthStatusMap(node)
	case []any:
		return modelAuthStatusSlice(node)
	}
	return false
}

func modelAuthStatusMap(node map[string]any) bool {
	for key, child := range node {
		if modelAuthStatusKey(key) && modelAuthStatusNumber(child) {
			return true
		}
		if modelAuthStatusNode(child) {
			return true
		}
	}
	return false
}

func modelAuthStatusSlice(node []any) bool {
	for _, child := range node {
		if modelAuthStatusNode(child) {
			return true
		}
	}
	return false
}

func modelAuthStatusKey(key string) bool {
	switch normalizedKey := strings.ToLower(strings.ReplaceAll(key, "-", "_")); normalizedKey {
	case "code", "status", "status_code", "statuscode", "http_status", "httpstatus", "error_code", "errorcode":
		return true
	default:
		return false
	}
}

func modelAuthStatusNumber(value any) bool {
	switch number := value.(type) {
	case float64:
		return number == 401 || number == 403
	case string:
		value := strings.TrimSpace(number)
		return value == "401" || value == "403"
	default:
		return false
	}
}

// ModelInterfaceUnavailable recognizes explicit CLI capability failures. A
// generic native nonzero exit is retained as failed so callers can distinguish
// a transient/provider failure from a command that cannot expose discovery.
func ModelInterfaceUnavailable(stdout, stderr []byte) bool {
	text := strings.ToLower(string(stdout) + "\n" + string(stderr))
	for _, marker := range []string{
		"unknown command", "no such command", "invalid subcommand",
		"unknown option", "unknown flag", "unrecognized option",
		"unsupported option", "option is not supported", "not supported",
		"command not found",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
