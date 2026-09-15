package claude

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPersonalOAuthProofExcludesCredentialsAndRejectsRemoteEligibility(t *testing.T) {
	deadline := time.Unix(2000000000, 0)
	fixture := func(plan string, expires int64, token string) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": token, "refreshToken": "fixture-refresh-secret", "subscriptionType": plan, "expiresAt": expires, "scopes": []string{"user:profile", "user:inference", "fixture-unrelated-secret"}}})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, plan := range []string{"pro", "max"} {
		proof, err := inspectPersonalOAuth(fixture(plan, deadline.Add(time.Hour).UnixMilli(), "fixture-access-secret"), deadline)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "Token") {
			t.Fatal("proof exposes credentials")
		}
	}
	cases := map[string][]byte{
		"team":                    fixture("team", deadline.Add(time.Hour).UnixMilli(), "fixture"),
		"enterprise":              fixture("enterprise", deadline.Add(time.Hour).UnixMilli(), "fixture"),
		"unknown":                 fixture("unknown", deadline.Add(time.Hour).UnixMilli(), "fixture"),
		"null plan":               []byte(`{"claudeAiOauth":{"subscriptionType":null}}`),
		"no token":                fixture("max", deadline.Add(time.Hour).UnixMilli(), ""),
		"expired":                 fixture("max", deadline.Add(-time.Second).UnixMilli(), "fixture"),
		"exact horizon":           fixture("max", deadline.UnixMilli(), "fixture"),
		"missing inference scope": []byte(`{"claudeAiOauth":{"accessToken":"fixture","subscriptionType":"max","expiresAt":2100000000000,"scopes":["user:profile"]}}`),
		"wrong-case native field": []byte(`{"claudeAiOauth":{"AccessToken":"fixture","SubscriptionType":"max","ExpiresAt":2100000000000,"Scopes":["user:inference"]}}`),
		"wrong-case native root":  []byte(`{"ClaudeAiOauth":{"accessToken":"fixture","subscriptionType":"max","expiresAt":2100000000000,"scopes":["user:inference"]}}`),
		"no login":                []byte(`{}`),
		"malformed":               []byte(`{`),
		"duplicate":               []byte(`{"claudeAiOauth":{},"claudeAiOauth":{}}`),
		"folded duplicate":        []byte(`{"claudeAiOauth":{},"ClaudeAiOauth":{}}`),
		"invalid UTF8":            {'{', '"', 'x', '"', ':', '"', 255, '"', '}'},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := inspectPersonalOAuth(data, deadline)
			if !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("got %v", err)
			}
			if strings.Contains(err.Error(), "fixture-access-secret") || strings.Contains(err.Error(), "fixture-refresh-secret") {
				t.Fatal("error exposes credentials")
			}
		})
	}
}
