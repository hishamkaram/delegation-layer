package antigravity

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseSettingsDefaults(t *testing.T) {
	defaults, err := ParseSettings([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.ToolPermission != ToolPermissionRequestReview || defaults.ArtifactReviewPolicy != ArtifactReviewAsksForReview || defaults.EnableTerminalSandbox || defaults.AllowNonWorkspaceAccess {
		t.Fatalf("defaults = %+v", defaults)
	}
	if err := ValidateSettings(Settings{}); err != nil {
		t.Fatalf("zero settings should use documented defaults: %v", err)
	}
}

func TestParseSettingsNormalizesExplicitEmptyApprovalDefaults(t *testing.T) {
	settings, err := ParseSettings([]byte(`{"toolPermission":"","artifactReviewPolicy":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if settings.ToolPermission != ToolPermissionRequestReview || settings.ArtifactReviewPolicy != ArtifactReviewAsksForReview {
		t.Fatalf("normalized settings = %+v", settings)
	}
	if !settings.HasToolPermission() || !settings.HasArtifactReviewPolicy() {
		t.Fatal("explicit default settings presence was not retained")
	}
	if err := ValidateSettings(Settings{ToolPermission: "", ArtifactReviewPolicy: ""}); err != nil {
		t.Fatalf("zero scalar settings should use documented defaults: %v", err)
	}
}

func TestParseSettingsSafeValues(t *testing.T) {
	data := []byte(`{
        "model": "Gemini 3 Pro (High)",
        "trustedWorkspaces": ["/workspace", "/workspace/child"],
        "toolPermission": "proceed-in-sandbox",
        "artifactReviewPolicy": "asks-for-review",
        "enableTerminalSandbox": false,
        "allowNonWorkspaceAccess": false,
        "permissions": {"allow": [], "ask": [], "deny": []}
    }`)
	got, err := ParseSettings(data)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		Model:                   "Gemini 3 Pro (High)",
		TrustedWorkspaces:       []string{"/workspace", "/workspace/child"},
		ToolPermission:          ToolPermissionProceedSandbox,
		ArtifactReviewPolicy:    ArtifactReviewAsksForReview,
		EnableTerminalSandbox:   false,
		AllowNonWorkspaceAccess: false,
		Permissions: PermissionSettings{
			Allow: []string{},
			Ask:   []string{},
			Deny:  []string{},
		},
	}
	want.present = settingsPresence{
		model:                   true,
		trustedWorkspaces:       true,
		toolPermission:          true,
		artifactReviewPolicy:    true,
		enableTerminalSandbox:   true,
		allowNonWorkspaceAccess: true,
		permissions:             true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	if !got.HasModel() || !got.HasTrustedWorkspaces() || !got.HasToolPermission() || !got.HasArtifactReviewPolicy() || !got.HasTerminalSandbox() || !got.HasNonWorkspaceAccess() || !got.HasPermissions() {
		t.Fatal("explicit settings presence was not retained")
	}
}

func TestParseSettingsRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{name: "tool bypass", data: `{"toolPermission":"always-proceed"}`, want: "always-proceed"},
		{name: "strict write mode", data: `{"toolPermission":"strict"}`, want: "strict"},
		{name: "artifact autonomy", data: `{"artifactReviewPolicy":"agent-decides"}`, want: "agent-decides"},
		{name: "artifact bypass", data: `{"artifactReviewPolicy":"always-proceed"}`, want: "always-proceed"},
		{name: "outside access", data: `{"allowNonWorkspaceAccess":true}`, want: "true"},
		{name: "allow rule", data: `{"permissions":{"allow":["read_file(/workspace)"]}}`, want: "read_file"},
		{name: "ask rule", data: `{"permissions":{"ask":["command(*)"]}}`, want: "command"},
		{name: "deny rule", data: `{"permissions":{"deny":["write_file(*)"]}}`, want: "write_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSettings([]byte(tc.data))
			assertSettingsError(t, err)
			if strings.Contains(err.Error(), tc.want) {
				t.Fatalf("settings error exposed raw value %q: %v", tc.want, err)
			}
		})
	}
}

func TestParseSettingsRejectsUnknownAndMalformedShape(t *testing.T) {
	invalid := []struct {
		name string
		data []byte
	}{
		{name: "unknown top-level key", data: []byte(`{"permission":{}}`)},
		{name: "case alias", data: []byte(`{"model":"one","Model":"two"}`)},
		{name: "underscore alias", data: []byte(`{"tool_permission":"request-review"}`)},
		{name: "unknown permissions key", data: []byte(`{"permissions":{"read":[]}}`)},
		{name: "null model", data: []byte(`{"model":null}`)},
		{name: "null workspace list", data: []byte(`{"trustedWorkspaces":null}`)},
		{name: "null permission list", data: []byte(`{"permissions":{"allow":null}}`)},
		{name: "null bool", data: []byte(`{"enableTerminalSandbox":null}`)},
		{name: "wrong string type", data: []byte(`{"toolPermission":true}`)},
		{name: "wrong bool type", data: []byte(`{"allowNonWorkspaceAccess":"false"}`)},
		{name: "wrong workspace type", data: []byte(`{"trustedWorkspaces":"/workspace"}`)},
		{name: "wrong permissions type", data: []byte(`{"permissions":[]}`)},
		{name: "workspace null element", data: []byte(`{"trustedWorkspaces":[null]}`)},
		{name: "trailing data", data: []byte(`{} {}`)},
		{name: "invalid UTF-8", data: []byte{'{', '"', 'm', 'o', 'd', 'e', 'l', '"', ':', '"', 'x', 0xff, '"', '}'}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSettings(tc.data)
			assertSettingsError(t, err)
		})
	}
}

func TestParseSettingsRejectsNonCanonicalWorkspacePaths(t *testing.T) {
	for _, path := range []string{"workspace", "/workspace/../other", "/workspace/", "/workspace/./child", "/workspace\u0000"} {
		t.Run(path, func(t *testing.T) {
			data := []byte(`{"trustedWorkspaces":["` + path + `"]}`)
			_, err := ParseSettings(data)
			assertSettingsError(t, err)
		})
	}
}

func TestParseSettingsUsesExactKeysAndNoFilesystemResolution(t *testing.T) {
	data := []byte(`{"trustedWorkspaces":["/path/that/does/not/exist"],"enableTerminalSandbox":true}`)
	got, err := parseSettings(data)
	if err != nil {
		t.Fatal(err)
	}
	if !got.EnableTerminalSandbox || len(got.TrustedWorkspaces) != 1 || got.TrustedWorkspaces[0] != "/path/that/does/not/exist" {
		t.Fatalf("settings = %+v", got)
	}
}

func assertSettingsError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("invalid settings were accepted")
	}
	if !errors.Is(err, ErrUnsupportedProfile) || !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("settings error = %v", err)
	}
}
