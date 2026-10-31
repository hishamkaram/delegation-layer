package provider

import (
	"bytes"
	"encoding/json"
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

// TestProviderExecutableClassifiesHelperByBothIdentityFields proves a
// definition is the provider CLI only when its top-level path and digest both
// match the runtime probe; anything else is a native helper whose digest is
// required.
func TestProviderExecutableClassifiesHelperByBothIdentityFields(t *testing.T) {
	definition, err := NewRuntimeInspectionDefinition(CLIInfo{Path: "/usr/bin/provider"}, "/workspace", []string{"LANG=C"}, RuntimeCapability{HelpArgs: []string{"exec"}})
	if err != nil {
		t.Fatal(err)
	}
	if !definition.ProviderExecutable() {
		t.Fatal("runtime-only provider definition was classified as a helper")
	}
	definition.Arguments = []string{"--native"}
	definition.Project = func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"ok":true}`), nil }
	definition.ExecutableSHA256 = strings.Repeat("b", 64)
	if definition.ProviderExecutable() {
		t.Fatal("same path with a different digest was classified as the provider")
	}
	if _, _, err = definition.Snapshot(); err != nil {
		t.Fatalf("helper definition with its digest was refused: %v", err)
	}
	definition.Executable, definition.ExecutableSHA256 = "/usr/bin/security", ""
	if _, _, err = definition.Snapshot(); err == nil {
		t.Fatal("native helper definition without a digest was accepted")
	}
}
