package codex

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEnvironmentPreservesCredentialLocationsWithoutInjectionControls(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(home, "existing codex home")
	values := []string{"HOME=" + home, "CODEX_HOME=" + codexHome, "PATH=/usr/bin:/bin", "DYLD_INSERT_LIBRARIES=/unsafe", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE=unrelated", "UNRELATED_SECRET=fixture-other"}
	got, err := prepareEnvironment(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range values[:3] {
		if !slices.Contains(got.Values, want) {
			t.Errorf("missing required environment coordinate %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	for _, unwanted := range values[3:] {
		if slices.Contains(got.Values, unwanted) {
			t.Error("retained unrelated environment control")
		}
	}
	if got.CodexHome != codexHome || !slices.Contains(got.WritableRoots, codexHome) {
		t.Fatal("native credential/runtime home was relocated")
	}
	for _, root := range got.WritableRoots {
		if strings.Contains(root, "fixture-credential") {
			t.Fatal("credential entered policy coordinates")
		}
	}
}

func TestEnvironmentRejectsAmbiguousOrRelativeCoordinates(t *testing.T) {
	home := t.TempDir()
	for _, values := range [][]string{
		{"HOME=" + home, "HOME=" + home},
		{"HOME=" + home, "CODEX_HOME=relative"},
		{"HOME=" + home, "TMPDIR=relative"},
		{"HOME=relative"},
		{"HOME=" + home, "malformed"},
		{"HOME=" + home, "CODEX_API_KEY=fixture-credential"},
		{"HOME=" + home, "CODEX_ACCESS_TOKEN=fixture-credential"},
		{"HOME=" + home, "OPENAI_API_KEY=fixture-credential"},
		{"HOME=" + home, "OPENAI_IDENTITY_TOKEN_FILE="},
		{"HOME=" + home, "PATH=invalid\x00value"},
	} {
		if _, err := prepareEnvironment(values); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("got %v, want unsupported environment", err)
		}
	}
}

func TestEnvironmentRejectsAlternateAuthEndpointsAndWorkloadIdentity(t *testing.T) {
	for _, key := range []string{"OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE", "OPENAI_WORKLOAD_IDENTITY_CONTEXT", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID"} {
		_, err := prepareEnvironment([]string{"HOME=" + t.TempDir(), key + "=private-fixture-value"})
		if !errors.Is(err, ErrUnsupportedProfile) || strings.Contains(err.Error(), "private-fixture-value") {
			t.Fatalf("selector %s was not rejected safely: %v", key, err)
		}
	}
}
