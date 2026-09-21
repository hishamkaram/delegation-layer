package codex

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type nativeFileAuth struct {
	Mode   string  `json:"auth_mode"`
	APIKey *string `json:"OPENAI_API_KEY"`
	Tokens struct {
		ID        string `json:"id_token"`
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		AccountID string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh   time.Time       `json:"last_refresh"`
	Agent         json.RawMessage `json:"agent_identity"`
	PersonalToken json.RawMessage `json:"personal_access_token"`
	BedrockKey    json.RawMessage `json:"bedrock_api_key"`
	BedrockKeys   json.RawMessage `json:"bedrock_access_keys"`
}

// authEligibility contains only policy-relevant, non-secret facts. Neither
// credentials nor a digest of their bytes may enter a persisted snapshot.
type authEligibility struct {
	Plan          string    `json:"plan"`
	AccessExpires int64     `json:"access_expires"`
	LastRefresh   time.Time `json:"last_refresh"`
}

func personalAuthSource(home string, now time.Time, budget time.Duration) (task.PolicySourceDigest, error) {
	path := filepath.Join(home, "auth.json")
	source, err := commonprovider.ReadPolicySource(path)
	if err != nil || !source.Present {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: %w: readable native file authentication is required", commonprovider.ErrAuthenticationBlocked, ErrUnsupportedProfile)
	}
	eligibility, err := inspectPersonalAuth(source.Data, now, budget)
	if err != nil {
		return task.PolicySourceDigest{}, err
	}
	data, err := task.MarshalCanonical(eligibility)
	if err != nil {
		return task.PolicySourceDigest{}, err
	}
	return task.PolicySourceDigest{Path: path, Kind: "personal-auth-eligibility", Present: true, SHA256: task.ComputeSHA256(data)}, nil
}

func inspectPersonalAuth(data []byte, now time.Time, budget time.Duration) (authEligibility, error) {
	var auth nativeFileAuth
	if !utf8.Valid(data) || task.ValidateJSONStructure(data) != nil {
		return authEligibility{}, authUnavailableError("invalid native authentication document")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&auth) != nil {
		return authEligibility{}, authUnavailableError("unsupported native authentication schema")
	}
	if (auth.Mode != "" && auth.Mode != "chatgpt") || auth.APIKey != nil || auth.Tokens.Access == "" || auth.Tokens.Refresh == "" {
		return authEligibility{}, authUnavailableError("persistent ChatGPT authentication is required")
	}
	for _, alternate := range []json.RawMessage{auth.Agent, auth.PersonalToken, auth.BedrockKey, auth.BedrockKeys} {
		if len(alternate) != 0 && !bytes.Equal(bytes.TrimSpace(alternate), []byte("null")) {
			return authEligibility{}, authUnavailableError("alternate authentication is unsupported")
		}
	}
	return personalTokenEligibility(auth, now, budget)
}

func personalTokenEligibility(auth nativeFileAuth, now time.Time, budget time.Duration) (authEligibility, error) {
	var identity struct {
		Auth struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	var access struct {
		Expires int64 `json:"exp"`
	}
	if decodeTokenClaims(auth.Tokens.ID, &identity) != nil || decodeTokenClaims(auth.Tokens.Access, &access) != nil {
		return authEligibility{}, authUnavailableError("unsupported native token claims")
	}
	switch identity.Auth.Plan {
	case "free", "go", "plus", "pro", "prolite":
	default:
		return authEligibility{}, authProfileError("only known personal plans have a verified no-cloud startup")
	}
	// Native auth() refreshes within five minutes of expiry or after eight
	// days since last refresh. Recheck before Start, with the task budget and
	// one minute of startup margin; never refresh credentials in preparation.
	windowEnd := now.Add(budget).Add(time.Minute)
	if budget <= 0 || !time.Unix(access.Expires, 0).After(windowEnd.Add(5*time.Minute)) ||
		auth.LastRefresh.IsZero() || auth.LastRefresh.After(now) || !auth.LastRefresh.After(windowEnd.Add(-8*24*time.Hour)) {
		return authEligibility{}, authUnavailableError("native authentication can refresh during the startup window; refresh it interactively first")
	}
	return authEligibility{Plan: identity.Auth.Plan, AccessExpires: access.Expires, LastRefresh: auth.LastRefresh}, nil
}

func decodeTokenClaims(token string, target any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return authUnavailableError("unsupported token envelope")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !utf8.Valid(data) || task.ValidateJSONStructure(data) != nil {
		return authUnavailableError("unsupported token payload")
	}
	if json.Unmarshal(data, target) != nil {
		return authUnavailableError("unsupported token claims")
	}
	return nil
}

func authProfileError(message string) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedProfile, message)
}

func authUnavailableError(message string) error {
	return fmt.Errorf("%w: %w: %s", commonprovider.ErrAuthenticationBlocked, ErrUnsupportedProfile, message)
}
