package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func nativePolicyFixture(t *testing.T, plan string, expiry int64) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "synthetic-access-secret", "refreshToken": "synthetic-refresh-secret",
		"subscriptionType": plan, "expiresAt": expiry, "scopes": []string{"user:inference", "synthetic-unrelated-secret"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNativeTeamProjectionRequiresFreshEmptyPolicy(t *testing.T) {
	horizon := time.Unix(2_000_000_000, 0)
	native := nativePolicyFixture(t, "team", horizon.Add(time.Hour).UnixMilli())
	authorization, required, err := nativePolicyAuthorization(native)
	if err != nil || !required || authorization != "Bearer synthetic-access-secret" {
		t.Fatal("Team fetch authorization missing")
	}
	for _, status := range []int{200, 204, 404} {
		facts, projectErr := projectNativePolicy(native, status, []byte(`{"uuid":"id","checksum":"digest","settings":{}}`))
		if projectErr != nil {
			t.Fatalf("empty status %d: %v", status, projectErr)
		}
		if strings.Contains(string(facts), "secret") || strings.Contains(string(facts), "Token") {
			t.Fatal("native projection leaked secrets")
		}
		decoded, decodeErr := decodePolicyFacts(facts, horizon)
		if decodeErr != nil || decoded.Subscription != "team" || decoded.RemotePolicy != "empty" {
			t.Fatalf("invalid Team facts: %v", decodeErr)
		}
	}
	for _, test := range []struct {
		status int
		body   string
	}{
		{0, ""},
		{304, ""},
		{401, ""},
		{500, ""},
		{200, `{}`},
		{200, `{"uuid":"x","checksum":"x","settings":{"hooks":{}}}`},
		{200, `{"uuid":null,"checksum":"x","settings":{}}`},
		{200, `{"uuid":"x","checksum":"x","settings":null}`},
		{200, `{"UUID":"x","checksum":"x","settings":{}}`},
		{200, `{"uuid":"x","checksum":"x","settings":{},"settings":{}}`},
	} {
		facts, projectErr := projectNativePolicy(native, test.status, []byte(test.body))
		if facts != nil || !errors.Is(projectErr, ErrUnsupportedProfile) {
			t.Fatalf("unsupported policy accepted: status=%d", test.status)
		}
	}
}

func TestNativePersonalProjectionRequiresNoRemoteObservation(t *testing.T) {
	for _, plan := range []string{"pro", "max"} {
		native := nativePolicyFixture(t, plan, 2_100_000_000_000)
		authorization, required, err := nativePolicyAuthorization(native)
		if err != nil || required || authorization != "" {
			t.Fatal("personal metadata unexpectedly requested network")
		}
		facts, err := projectNativePolicy(native, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodePolicyFacts(facts, time.Unix(2_000_000_000, 0)); err != nil {
			t.Fatal(err)
		}
		if _, err = projectNativePolicy(native, 404, nil); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatal("personal facts accepted unexpected network observation")
		}
	}
}

func TestNativePolicyRejectsMalformedAuthWithoutDiagnostics(t *testing.T) {
	for _, data := range [][]byte{
		nativePolicyFixture(t, "enterprise", 2_100_000_000_000), nativePolicyFixture(t, "unknown", 2_100_000_000_000),
		[]byte(`{"claudeAiOauth":{},"claudeAiOauth":{}}`), []byte(`{"ClaudeAiOauth":{}}`), []byte(`null`),
		[]byte(strings.ReplaceAll(string(nativePolicyFixture(t, "team", 2_100_000_000_000)), "synthetic-access-secret", `synthetic\r\nsecret`)),
	} {
		authorization, required, err := nativePolicyAuthorization(data)
		if authorization != "" || required || !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatal("invalid native metadata accepted")
		}
		if strings.Contains(err.Error(), "synthetic") {
			t.Fatal("diagnostic leaked native data")
		}
	}
}

func TestPolicyFactsAreStrictAndExpiryIsNotRenewed(t *testing.T) {
	horizon := time.Unix(2_000_000_000, 0)
	facts, err := projectNativePolicy(nativePolicyFixture(t, "team", horizon.UnixMilli()), 404, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = decodePolicyFacts(facts, horizon); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatal("exact expiry boundary accepted")
	}
	valid, err := projectNativePolicy(nativePolicyFixture(t, "team", horizon.Add(time.Hour).UnixMilli()), 404, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Replace(string(valid), "subscription", "Subscription", 1),
		strings.Replace(string(valid), `"inference_scope":true`, `"inference_scope":null`, 1),
		strings.Replace(string(valid), `"remote_policy":"empty"`, `"remote_policy":"ineligible"`, 1),
		strings.TrimSpace(string(valid))[:len(strings.TrimSpace(string(valid)))-1] + `,"unexpected":"secret"}`,
	} {
		if _, err = decodePolicyFacts([]byte(invalid), horizon); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatal("unknown or malformed proof accepted")
		}
	}
}

func TestPolicyDigestBindsFactsWithoutObservationTime(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	facts, err := projectNativePolicy(nativePolicyFixture(t, "team", now.Add(time.Hour).UnixMilli()), 404, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{CanonicalCwd: "/workspace", BudgetNanos: int64(time.Minute)}
	environment := profileEnvironment{WritableRoots: []string{"/home/test/.claude"}, RuntimeSHA256: strings.Repeat("c", 64)}
	digest := strings.Repeat("a", 64)
	first, err := finalizePolicy(request, environment, nil, digest, facts, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := finalizePolicy(request, environment, nil, digest, facts, now.Add(time.Second))
	if err != nil || !task.CompareEffectiveConfigs(first, second) {
		t.Fatal("observation timestamp changed effective policy")
	}
	changed, err := finalizePolicy(request, environment, nil, strings.Repeat("b", 64), facts, now)
	if err != nil || task.CompareEffectiveConfigs(first, changed) {
		t.Fatal("definition drift was not bound")
	}
	if _, err = finalizePolicy(request, environment, nil, digest, facts, now.Add(59*time.Minute)); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatal("auth horizon omitted budget and refresh margin")
	}
}

func TestNativeAuthorizationRefusesMalformedTokenBeforeNetwork(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	for _, token := range []string{"", " ", "\t", "\n", "secret token", "\u2003", "secret\x7f", "=", "=abc", "abc=def"} {
		t.Run(fmt.Sprintf("token-%x", token), func(t *testing.T) {
			var record map[string]any
			if err := json.Unmarshal(nativePolicyFixture(t, "team", now.Add(time.Hour).UnixMilli()), &record); err != nil {
				t.Fatal(err)
			}
			oauth, ok := record["claudeAiOauth"].(map[string]any)
			if !ok {
				t.Fatal("invalid test OAuth fixture")
			}
			oauth["accessToken"] = token
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			authorization, required, err := nativePolicyAuthorizationAt(data, now)
			if authorization != "" || required || !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatal("malformed credential crossed network authorization boundary")
			}
		})
	}
}

func TestNativeAuthorizationRefusesRefreshBoundaryBeforeNetwork(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	for _, untilExpiry := range []time.Duration{-time.Second, 0, refreshMargin - time.Millisecond, refreshMargin} {
		native := nativePolicyFixture(t, "team", now.Add(untilExpiry).UnixMilli())
		authorization, required, err := nativePolicyAuthorizationAt(native, now)
		if authorization != "" || required || !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("expiry %s crossed network authorization boundary", untilExpiry)
		}
	}
	native := nativePolicyFixture(t, "team", now.Add(refreshMargin+time.Millisecond).UnixMilli())
	authorization, required, err := nativePolicyAuthorizationAt(native, now)
	if err != nil || !required || authorization == "" {
		t.Fatal("fresh credential could not authorize policy inspection")
	}
}

func TestNativeAccessTokenAllowsBearerAlphabetAndTrailingPadding(t *testing.T) {
	for _, token := range []string{"abcABC019-._~+/", "abc=", "abc=="} {
		if !validNativeAccessToken(token) {
			t.Fatal("valid bearer token syntax refused")
		}
	}
}
