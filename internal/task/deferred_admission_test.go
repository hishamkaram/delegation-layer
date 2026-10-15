package task

import (
	"bytes"
	"testing"
)

func TestMetaRecordAllowsPendingRuntimePreflightWithExplicitEmptyEnvironment(t *testing.T) {
	meta := fixtureMeta(t)
	meta.ProviderVersion = ""
	meta.RuntimePreflightSHA256 = ComputeSHA256([]byte("runtime-capability"))
	meta.Environment = nil
	meta.EnvironmentRecorded = true

	if err := ValidateMetaRecord(&meta); err != nil {
		t.Fatalf("valid pending runtime preflight was rejected: %v", err)
	}

	encoded, err := MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"runtime_preflight_sha256":"`+meta.RuntimePreflightSHA256+`"`)) {
		t.Fatalf("canonical meta omitted the authoritative runtime preflight digest: %s", encoded)
	}
	var decoded MetaRecord
	if err := DecodeStrict(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RuntimePreflightSHA256 != meta.RuntimePreflightSHA256 || decoded.ProviderVersion != "" || !decoded.EnvironmentRecorded {
		t.Fatalf("pending runtime preflight was not preserved: decoded=%+v", decoded)
	}
	if err := ValidateMetaRecord(&decoded); err != nil {
		t.Fatalf("round-tripped pending runtime preflight was rejected: %v", err)
	}
}

func TestMetaRecordRejectsInvalidPendingRuntimePreflightCombinations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*MetaRecord)
	}{
		{
			name: "empty version without marker",
			mutate: func(meta *MetaRecord) {
				meta.ProviderVersion = ""
			},
		},
		{
			name: "empty digest with marker",
			mutate: func(meta *MetaRecord) {
				meta.ProviderVersion = ""
				meta.EnvironmentRecorded = true
			},
		},
		{
			name: "invalid digest",
			mutate: func(meta *MetaRecord) {
				meta.ProviderVersion = ""
				meta.RuntimePreflightSHA256 = "not-a-sha256"
				meta.EnvironmentRecorded = true
			},
		},
		{
			name: "missing environment marker",
			mutate: func(meta *MetaRecord) {
				meta.ProviderVersion = ""
				meta.RuntimePreflightSHA256 = ComputeSHA256([]byte("runtime-capability"))
				meta.EnvironmentRecorded = false
			},
		},
		{
			name: "observed version with pending digest",
			mutate: func(meta *MetaRecord) {
				meta.ProviderVersion = "provider-v1"
				meta.RuntimePreflightSHA256 = ComputeSHA256([]byte("runtime-capability"))
				meta.EnvironmentRecorded = true
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			meta := fixtureMeta(t)
			testCase.mutate(&meta)
			if err := ValidateMetaRecord(&meta); err == nil {
				t.Fatal("invalid pending runtime preflight was accepted")
			}
		})
	}
}
