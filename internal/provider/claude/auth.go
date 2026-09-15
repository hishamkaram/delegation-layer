package claude

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// personalOAuthProof contains only the native eligibility fields needed to
// prove the pinned remote-settings loader skips cache and network access.
// Tokens remain transient and are never returned, hashed, or persisted.
type personalOAuthProof struct {
	SubscriptionType string   `json:"subscription_type"`
	ExpiresAt        int64    `json:"expires_at"`
	Scopes           []string `json:"scopes"`
}

func inspectPersonalOAuth(data []byte, requiredUntil time.Time) (personalOAuthProof, error) {
	if !utf8.Valid(data) || task.ValidateJSONStructure(data) != nil {
		return personalOAuthProof{}, fmt.Errorf("%w: malformed native OAuth storage", ErrUnsupportedProfile)
	}
	oauth, err := decodeNativeOAuth(data)
	if err != nil {
		return personalOAuthProof{}, fmt.Errorf("%w: native OAuth login metadata is unavailable", ErrUnsupportedProfile)
	}
	if oauth.AccessToken == "" || (oauth.SubscriptionType != "pro" && oauth.SubscriptionType != "max") || !slices.Contains(oauth.Scopes, "user:inference") {
		return personalOAuthProof{}, fmt.Errorf("%w: known personal Pro/Max native OAuth is required", ErrUnsupportedProfile)
	}
	if oauth.ExpiresAt <= requiredUntil.UnixMilli() {
		return personalOAuthProof{}, fmt.Errorf("%w: native OAuth lifetime does not cover the task and refresh margin", ErrUnsupportedProfile)
	}
	// Retain only the scope predicate used by this profile, never arbitrary
	// strings from credential storage or unrelated granted scopes.
	return personalOAuthProof{SubscriptionType: oauth.SubscriptionType, ExpiresAt: oauth.ExpiresAt, Scopes: []string{"user:inference"}}, nil
}

// Native JavaScript property lookup is case-sensitive. Go struct decoding is
// not, so select the exact native keys before decoding their values.
type nativeOAuthData struct {
	AccessToken      string
	SubscriptionType string
	ExpiresAt        int64
	Scopes           []string
}

func decodeNativeOAuth(data []byte) (nativeOAuthData, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nativeOAuthData{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(root["claudeAiOauth"], &fields); err != nil {
		return nativeOAuthData{}, err
	}
	var oauth nativeOAuthData
	for _, field := range []struct {
		name  string
		value any
	}{
		{"accessToken", &oauth.AccessToken},
		{"subscriptionType", &oauth.SubscriptionType},
		{"expiresAt", &oauth.ExpiresAt},
		{"scopes", &oauth.Scopes},
	} {
		if err := json.Unmarshal(fields[field.name], field.value); err != nil {
			return nativeOAuthData{}, err
		}
	}
	return oauth, nil
}
