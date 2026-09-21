package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func fixtureJWT(t *testing.T, claims any) string {
	t.Helper()
	data, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "fixture-header." + base64.RawURLEncoding.EncodeToString(data) + ".fixture-signature"
}

func fixtureAuth(t *testing.T, now time.Time, plan string) nativeFileAuth {
	t.Helper()
	auth := nativeFileAuth{Mode: "chatgpt", LastRefresh: now.Add(-time.Hour)}
	auth.Tokens.ID = fixtureJWT(t, map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": plan}, "email": "private-fixture@example.invalid"})
	auth.Tokens.Access = fixtureJWT(t, map[string]int64{"exp": now.Add(time.Hour).Unix()})
	auth.Tokens.Refresh = "private-fixture-refresh-token"
	auth.Tokens.AccountID = "private-fixture-account"
	return auth
}

func marshalAuth(t *testing.T, auth nativeFileAuth) []byte {
	t.Helper()
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPersonalAuthRecognizesOnlyNonCloudPlans(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, plan := range []string{"free", "go", "plus", "pro", "prolite"} {
		auth := fixtureAuth(t, now, plan)
		got, err := inspectPersonalAuth(marshalAuth(t, auth), now, 2*time.Minute)
		if err != nil || got.Plan != plan {
			t.Fatalf("plan %s: %+v %v", plan, got, err)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "private-fixture") {
			t.Fatal("credential or account identity escaped into eligibility")
		}
	}
	for _, plan := range []string{"", "business", "enterprise", "edu", "unknown", "future-plan"} {
		if _, err := inspectPersonalAuth(marshalAuth(t, fixtureAuth(t, now, plan)), now, time.Minute); !errors.Is(err, ErrUnsupportedProfile) || errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
			t.Fatalf("accepted unverified plan %q: %v", plan, err)
		}
	}
}

func TestPersonalAuthRejectsRefreshWindowsAndAlternateSelection(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	cases := map[string]func(*nativeFileAuth){
		"expiry boundary": func(a *nativeFileAuth) {
			a.Tokens.Access = fixtureJWT(t, map[string]int64{"exp": now.Add(8 * time.Minute).Unix()})
		},
		"missing expiry": func(a *nativeFileAuth) { a.Tokens.Access = fixtureJWT(t, map[string]string{}) },
		"expired": func(a *nativeFileAuth) {
			a.Tokens.Access = fixtureJWT(t, map[string]int64{"exp": now.Add(-time.Hour).Unix()})
		},
		"refresh age boundary":  func(a *nativeFileAuth) { a.LastRefresh = now.Add(-8*24*time.Hour + 3*time.Minute) },
		"missing refresh":       func(a *nativeFileAuth) { a.LastRefresh = time.Time{} },
		"future refresh":        func(a *nativeFileAuth) { a.LastRefresh = now.Add(time.Minute) },
		"alternate mode":        func(a *nativeFileAuth) { a.Mode = "chatgptAuthTokens" },
		"API key":               func(a *nativeFileAuth) { value := "private-fixture-api-key"; a.APIKey = &value },
		"agent identity":        func(a *nativeFileAuth) { a.Agent = json.RawMessage(`"private-fixture-agent"`) },
		"personal access token": func(a *nativeFileAuth) { a.PersonalToken = json.RawMessage(`"private-fixture-pat"`) },
		"bedrock":               func(a *nativeFileAuth) { a.BedrockKey = json.RawMessage(`{}`) },
		"bad token":             func(a *nativeFileAuth) { a.Tokens.ID = "private-fixture-malformed-token" },
		"missing refresh token": func(a *nativeFileAuth) { a.Tokens.Refresh = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			auth := fixtureAuth(t, now, "pro")
			mutate(&auth)
			_, err := inspectPersonalAuth(marshalAuth(t, auth), now, 2*time.Minute)
			if !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("expected rejection, got %v", err)
			}
			if strings.Contains(err.Error(), "private-fixture") {
				t.Fatal("authentication error disclosed secret data")
			}
		})
	}
}

func TestAuthDigestContainsEligibilityRatherThanCredentials(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	auth := fixtureAuth(t, now, "pro")
	write := func() {
		t.Helper()
		if writeErr := os.WriteFile(filepath.Join(home, "auth.json"), marshalAuth(t, auth), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	write()
	first, err := personalAuthSource(home, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	auth.Tokens.Refresh = "different-private-fixture-refresh-token"
	auth.Tokens.AccountID = "different-private-fixture-account"
	write()
	second, err := personalAuthSource(home, now, time.Minute)
	if err != nil || second != first {
		t.Fatalf("credential bytes changed eligibility receipt: %v", err)
	}
	auth.LastRefresh = now.Add(-2 * time.Hour)
	write()
	third, err := personalAuthSource(home, now, time.Minute)
	if err != nil || third.SHA256 == first.SHA256 {
		t.Fatalf("eligibility drift was not bound: %v", err)
	}
}

func TestAuthRejectsAmbiguousDocumentsWithoutEchoingData(t *testing.T) {
	now := time.Now()
	for _, data := range [][]byte{
		[]byte(`{"auth_mode":"chatgpt","auth_mode":"apikey","private-fixture":"secret"}`),
		[]byte(`{"unexpected":"private-fixture-secret"}`),
		[]byte(`null`),
		[]byte("{\"private-fixture\":\"\xff\"}"),
	} {
		_, err := inspectPersonalAuth(data, now, time.Minute)
		if !errors.Is(err, ErrUnsupportedProfile) || strings.Contains(err.Error(), "private-fixture") {
			t.Fatalf("unsafe authentication parse: %v", err)
		}
	}
}
