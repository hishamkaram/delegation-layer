package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRuntimeFactsVersionAndDigestAreOptionalObservations(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, facts := range []RuntimeFacts{
		{Executable: "/usr/bin/provider"},
		{Executable: "/usr/bin/provider", Version: "provider-v1"},
		{Executable: "/usr/bin/provider", SHA256: digest},
		{Executable: "/usr/bin/provider", Version: "provider-v1", SHA256: digest, CapabilitySHA256: digest},
	} {
		if err := ValidateRuntimeFacts(facts); err != nil {
			t.Fatalf("optional runtime observation %+v rejected: %v", facts, err)
		}
	}
}

func TestRuntimeFactsRejectMalformedPresentObservations(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, facts := range []RuntimeFacts{
		{},
		{Version: "provider-v1", SHA256: digest},
		{Executable: "provider"},
		{Executable: "/usr/bin/../bin/provider"},
		{Executable: "/usr/bin/provider", SHA256: "invalid"},
		{Executable: "/usr/bin/provider", SHA256: strings.Repeat("A", 64)},
		{Executable: "/usr/bin/provider", CapabilitySHA256: "invalid"},
		{Executable: "/usr/bin/provider", Version: " "},
		{Executable: "/usr/bin/provider", Version: "provider\x1bversion"},
		{Executable: "/usr/bin/provider", Version: string([]byte{'p', 0xff})},
		{Executable: "/usr/bin/provider", Version: strings.Repeat("v", task.MaxProviderRuntimeEvidenceVersionBytes+1)},
	} {
		if err := ValidateRuntimeFacts(facts); err == nil {
			t.Fatalf("malformed runtime observation %+v was accepted", facts)
		}
	}
}

// TestInspectionFactsCanonicalBytes pins the envelope wire form: version and
// sha256 stay required JSON members, and an empty value means not observed.
func TestInspectionFactsCanonicalBytes(t *testing.T) {
	digest := strings.Repeat("a", 64)
	cases := []struct {
		name  string
		facts RuntimeFacts
		want  string
	}{
		{
			name:  "historical-populated",
			facts: RuntimeFacts{Executable: "/usr/bin/provider", Version: "provider-v1", SHA256: digest, CapabilitySHA256: digest},
			want:  `{"runtime":{"executable":"/usr/bin/provider","version":"provider-v1","sha256":"` + digest + `","capability_sha256":"` + digest + `"}}` + "\n",
		},
		{
			name:  "path-only",
			facts: RuntimeFacts{Executable: "/usr/bin/provider"},
			want:  `{"runtime":{"executable":"/usr/bin/provider","version":"","sha256":""}}` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := EncodeInspectionFacts(tc.facts, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("encoded facts=%s, want %s", encoded, tc.want)
			}
			decoded, err := DecodeInspectionFacts(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Runtime == nil || *decoded.Runtime != tc.facts {
				t.Fatalf("decoded facts=%+v, want %+v", decoded.Runtime, tc.facts)
			}
			again, err := EncodeInspectionFacts(*decoded.Runtime, decoded.Native)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, again) {
				t.Fatalf("canonical facts changed after round trip:\n%s\n%s", encoded, again)
			}
		})
	}
}

func TestDecodeInspectionFactsKeepsStrictRuntimeMembers(t *testing.T) {
	for _, data := range []string{
		`{"runtime":{"executable":"/usr/bin/provider","sha256":""}}`,
		`{"runtime":{"executable":"/usr/bin/provider","version":""}}`,
		`{"runtime":{"executable":"/usr/bin/provider","version":null,"sha256":""}}`,
		`{"runtime":{"executable":"/usr/bin/provider","version":"","sha256":"","extra":""}}`,
		`{"runtime":{"executable":"/usr/bin/provider","version":"","sha256":"","sha256":""}}`,
		`{"runtime":{"executable":"/usr/bin/provider","version":"","sha256":"invalid"}}`,
	} {
		if _, err := DecodeInspectionFacts([]byte(data)); err == nil {
			t.Fatalf("malformed runtime facts were accepted: %s", data)
		}
	}
}

// Fresh installed roles have no executable digest. A native projection can use
// its own helper, while a runtime-only definition must retain the provider role.
func TestProviderExecutableClassifiesInstalledRolesWithoutDigests(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		executable string
		native     bool
		provider   bool
		refused    bool
	}{
		{name: "provider runtime", executable: "/usr/bin/provider", provider: true},
		{name: "provider native projection", executable: "/usr/bin/provider", native: true, provider: true},
		{name: "installed native helper", executable: "/usr/bin/security", native: true},
		{name: "runtime-only helper conflict", executable: "/usr/bin/security", refused: true},
		{name: "relative helper refused", executable: "security", native: true, refused: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			definition, err := NewRuntimeInspectionDefinition(CLIInfo{Path: "/usr/bin/provider"}, "/workspace", []string{"LANG=C"}, RuntimeCapability{HelpArgs: []string{"exec"}})
			if err != nil {
				t.Fatal(err)
			}
			definition.Executable = testCase.executable
			if testCase.native {
				definition.Arguments = []string{"--native"}
				definition.Project = func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"ok":true}`), nil }
			}
			if definition.ProviderExecutable() != testCase.provider {
				t.Fatalf("installed executable %q provider role=%t, want %t", definition.Executable, definition.ProviderExecutable(), testCase.provider)
			}
			if testCase.refused {
				_, _, err = definition.Snapshot()
				if !errors.Is(err, ErrProfileUnavailable) {
					t.Fatalf("invalid installed role error=%v, want %v", err, ErrProfileUnavailable)
				}
				return
			}
			assertInstalledRoleSnapshot(t, definition, testCase.provider)
		})
	}
}

func assertInstalledRoleSnapshot(t *testing.T, definition InspectionDefinition, wantProvider bool) {
	t.Helper()
	snapshot, _, err := definition.Snapshot()
	if err != nil {
		t.Fatalf("installed role without executable digest was refused: %v", err)
	}
	if snapshot.Executable != definition.Executable || snapshot.ExecutableSHA256 != "" || snapshot.Runtime == nil ||
		snapshot.Runtime.Executable != "/usr/bin/provider" || snapshot.Runtime.ExecutableSHA256 != "" || snapshot.ProviderExecutable() != wantProvider {
		t.Fatalf("snapshot changed installed role metadata: %+v", snapshot)
	}
}
