package claude

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	nativeInspectionRevision = "claude-native-oauth-policy-v1"
	nativeCredentialHelper   = "/usr/bin/security"
	claudeSettingsEndpoint   = "https://api.anthropic.com/api/claude_code/settings"
	refreshMargin            = 5 * time.Minute
)

// nativePolicyFacts is the entire persisted projection. Arbitrary native keys,
// tokens, granted scope strings, and server response fields never enter it.
type nativePolicyFacts struct {
	SchemaVersion  int    `json:"schema_version"`
	Subscription   string `json:"subscription"`
	ExpiresAt      int64  `json:"expires_at"`
	InferenceScope bool   `json:"inference_scope"`
	RemotePolicy   string `json:"remote_policy"`
}

func nativeInspection(environment profileEnvironment) (commonprovider.InspectionDefinition, error) {
	account, err := legacyAPIKeyAccount(environment)
	if err != nil {
		return commonprovider.InspectionDefinition{}, unsupportedNativeFacts()
	}
	helperDigest, err := commonprovider.FingerprintExecutable(nativeCredentialHelper)
	if err != nil {
		return commonprovider.InspectionDefinition{}, unsupportedNativeFacts()
	}
	definition := commonprovider.InspectionDefinition{
		Revision: nativeInspectionRevision, Executable: nativeCredentialHelper, ExecutableSHA256: helperDigest,
		Arguments: []string{"find-generic-password", "-s", "Claude Code-credentials", "-a", account, "-w"},
		Directory: environment.Home, Environment: slices.Clone(environment.Values), OutputLimit: commonprovider.MaxInspectionOutput,
		Remote: &commonprovider.HTTPInspectionDefinition{
			URL: claudeSettingsEndpoint,
			Headers: map[string]string{
				"anthropic-beta": "oauth-2025-04-20", "User-Agent": "claude-cli (external, cli)",
				"Cache-Control": "no-cache", "Pragma": "no-cache",
			},
			Authorization: nativePolicyAuthorization,
			Project:       projectNativePolicy,
		},
	}
	definition, _, err = definition.Snapshot()
	return definition, err
}

func unsupportedNativeFacts() error {
	return fmt.Errorf("%w: native authentication and policy proof is unavailable", ErrUnsupportedProfile)
}

func supportedNativeOAuth(data []byte) (nativeOAuthData, error) {
	if !utf8.Valid(data) || task.ValidateJSONStructure(data) != nil {
		return nativeOAuthData{}, unsupportedNativeFacts()
	}
	oauth, err := decodeNativeOAuth(data)
	if err != nil || !validNativeAccessToken(oauth.AccessToken) || oauth.ExpiresAt <= 0 || !slices.Contains(oauth.Scopes, "user:inference") {
		return nativeOAuthData{}, unsupportedNativeFacts()
	}
	if oauth.SubscriptionType != "pro" && oauth.SubscriptionType != "max" && oauth.SubscriptionType != "team" {
		return nativeOAuthData{}, unsupportedNativeFacts()
	}
	return oauth, nil
}

// validNativeAccessToken enforces RFC 6750 section 2.1's bearer token syntax.
// Padding is allowed only after a nonempty token body.
func validNativeAccessToken(token string) bool {
	token = strings.TrimRight(token, "=")
	if token == "" {
		return false
	}
	for _, ch := range token {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~+/", ch) {
			continue
		}
		return false
	}
	return true
}

func nativePolicyAuthorization(native []byte) (string, bool, error) {
	return nativePolicyAuthorizationAt(native, time.Now())
}

func nativePolicyAuthorizationAt(native []byte, now time.Time) (string, bool, error) {
	oauth, err := supportedNativeOAuth(native)
	if err != nil {
		return "", false, err
	}
	if now.IsZero() || !time.UnixMilli(oauth.ExpiresAt).After(now.Add(refreshMargin)) {
		return "", false, unsupportedNativeFacts()
	}
	if oauth.SubscriptionType != "team" {
		return "", false, nil
	}
	return "Bearer " + oauth.AccessToken, true, nil
}

func projectNativePolicy(native []byte, status int, body []byte) (json.RawMessage, error) {
	oauth, err := supportedNativeOAuth(native)
	if err != nil {
		return nil, err
	}
	facts := nativePolicyFacts{SchemaVersion: 1, Subscription: oauth.SubscriptionType, ExpiresAt: oauth.ExpiresAt, InferenceScope: true}
	if oauth.SubscriptionType == "team" {
		if !emptyRemotePolicy(status, body) {
			return nil, unsupportedNativeFacts()
		}
		facts.RemotePolicy = "empty"
	} else {
		if status != 0 || len(body) != 0 {
			return nil, unsupportedNativeFacts()
		}
		facts.RemotePolicy = "ineligible"
	}
	return task.MarshalCanonical(facts)
}

// The pinned native loader treats 204/404 as empty and validates a 200 envelope
// containing uuid/checksum/settings. Other statuses, cache fallbacks, and
// nonempty managed settings cannot qualify this baseline.
func emptyRemotePolicy(status int, body []byte) bool {
	if status == 204 || status == 404 {
		return true
	}
	if status != 200 || !utf8.Valid(body) || task.ValidateJSONStructure(body) != nil {
		return false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return false
	}
	for _, key := range []string{"uuid", "checksum"} {
		var value *string
		if json.Unmarshal(envelope[key], &value) != nil || value == nil {
			return false
		}
	}
	var settings map[string]json.RawMessage
	return json.Unmarshal(envelope["settings"], &settings) == nil && settings != nil && len(settings) == 0
}

func decodePolicyFacts(data []byte, requiredUntil time.Time) (nativePolicyFacts, error) {
	if len(data) > 4096 || !utf8.Valid(data) || task.ValidateJSONStructure(data) != nil {
		return nativePolicyFacts{}, unsupportedNativeFacts()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 5 {
		return nativePolicyFacts{}, unsupportedNativeFacts()
	}
	var facts nativePolicyFacts
	for _, field := range []struct {
		name        string
		destination any
	}{
		{"schema_version", &facts.SchemaVersion},
		{"subscription", &facts.Subscription},
		{"expires_at", &facts.ExpiresAt},
		{"inference_scope", &facts.InferenceScope},
		{"remote_policy", &facts.RemotePolicy},
	} {
		raw, exists := fields[field.name]
		if !exists || string(raw) == "null" || json.Unmarshal(raw, field.destination) != nil {
			return nativePolicyFacts{}, unsupportedNativeFacts()
		}
	}
	if err := validatePolicyFacts(facts, requiredUntil); err != nil {
		return nativePolicyFacts{}, err
	}
	return facts, nil
}

func validatePolicyFacts(facts nativePolicyFacts, requiredUntil time.Time) error {
	if facts.SchemaVersion != 1 || !facts.InferenceScope || requiredUntil.IsZero() || facts.ExpiresAt <= requiredUntil.UnixMilli() {
		return unsupportedNativeFacts()
	}
	if facts.Subscription == "team" && facts.RemotePolicy == "empty" {
		return nil
	}
	if (facts.Subscription == "pro" || facts.Subscription == "max") && facts.RemotePolicy == "ineligible" {
		return nil
	}
	return unsupportedNativeFacts()
}
