package task

import (
	"bytes"
	"strings"
	"testing"
)

func TestProviderRuntimeIdentityAcceptsOptionalObservations(t *testing.T) {
	digest := ComputeSHA256([]byte("provider"))
	for _, identity := range []ProviderRuntimeIdentity{
		{},
		{Executable: "/provider"},
		{Executable: "/provider", Version: "provider-v1"},
		{Executable: "/provider", SHA256: digest},
		{Executable: "/provider", Version: "provider-v1", SHA256: digest},
	} {
		if err := ValidateProviderRuntimeIdentity(identity); err != nil {
			t.Fatalf("optional provider observation %+v rejected: %v", identity, err)
		}
		if err := ValidateProviderRuntimeEvidenceIdentity(identity); err != nil {
			t.Fatalf("optional provider evidence %+v rejected: %v", identity, err)
		}
	}
}

func TestProviderRuntimeIdentityRejectsMalformedPresentObservations(t *testing.T) {
	digest := ComputeSHA256([]byte("provider"))
	for _, identity := range []ProviderRuntimeIdentity{
		{Version: "provider-v1"},
		{SHA256: digest},
		{Version: "provider-v1", SHA256: digest},
		{Executable: "provider", Version: "provider-v1", SHA256: digest},
		{Executable: "/provider/../provider"},
		{Executable: "/provider", SHA256: "invalid"},
		{Executable: "/provider", SHA256: strings.ToUpper(digest)},
		{Executable: "/provider", SHA256: digest[:63]},
		{Executable: "/provider", Version: " "},
		{Executable: "/provider", Version: "provider\nversion"},
		{Executable: "/provider", Version: string([]byte{'p', 0xff, 'v'})},
	} {
		if err := ValidateProviderRuntimeIdentity(identity); err == nil {
			t.Fatalf("malformed provider observation %+v was accepted", identity)
		}
		if err := ValidateProviderRuntimeEvidenceIdentity(identity); err == nil {
			t.Fatalf("malformed provider evidence %+v was accepted", identity)
		}
	}
}

func TestPolicyRuntimeDigestIsOptionalObservation(t *testing.T) {
	config := EffectiveConfig{Containment: "workspace", Approval: "never", Digest: effectivePolicyTestDigest, Policy: validPolicyDetailsForTest()}
	config.Policy.RuntimeSHA256 = ""
	if err := ValidateEffectiveConfig(config); err != nil {
		t.Fatalf("policy without provider runtime digest rejected: %v", err)
	}
	observed := CloneEffectiveConfig(config)
	observed.Policy.RuntimeSHA256 = effectivePolicyTestDigest
	if CompareEffectiveConfigs(config, observed) {
		t.Fatal("absent and present provider runtime digests compared equal")
	}
	for _, digest := range []string{" ", "invalid", strings.ToUpper(effectivePolicyTestDigest)} {
		config.Policy.RuntimeSHA256 = digest
		if err := ValidateEffectiveConfig(config); err == nil {
			t.Fatalf("malformed present policy runtime digest %q was accepted", digest)
		}
	}
}

func TestMetaProviderVersionIsOptionalObservation(t *testing.T) {
	record := fixtureMeta(t)
	record.ProviderVersion = ""
	if err := ValidateMetaRecord(&record); err != nil {
		t.Fatalf("observed meta without provider version rejected: %v", err)
	}
	for _, version := range []string{" ", "\t"} {
		record.ProviderVersion = version
		if err := ValidateMetaRecord(&record); err == nil {
			t.Fatalf("blank present provider version %q was accepted", version)
		}
	}
	pending := fixtureMeta(t)
	pending.ProviderVersion = ""
	pending.RuntimePreflightSHA256 = ComputeSHA256([]byte("capability"))
	pending.EnvironmentRecorded = true
	if err := ValidateMetaRecord(&pending); err != nil {
		t.Fatalf("pending meta rejected: %v", err)
	}
	pending.ProviderVersion = "provider-v1"
	if err := ValidateMetaRecord(&pending); err == nil {
		t.Fatal("pending meta with an observed provider version was accepted")
	}
}

// TestProviderObservationRecordsRoundTripCanonically pins the persisted forms:
// meta always encodes provider_version (empty when unobserved), while start and
// seal records omit absent provider observation fields.
func TestProviderObservationRecordsRoundTripCanonically(t *testing.T) {
	pending := fixtureMeta(t)
	pending.ProviderVersion = ""
	pending.RuntimePreflightSHA256 = ComputeSHA256([]byte("capability"))
	pending.EnvironmentRecorded = true
	observed := fixtureMeta(t)
	observed.ProviderVersion = ""
	for name, record := range map[string]MetaRecord{"pending": pending, "observed-without-version": observed} {
		data := assertCanonicalRoundTrip(t, record, func(r *MetaRecord) error { return ValidateMetaRecord(r) })
		if !bytes.Contains(data, []byte(`"provider_version":""`)) {
			t.Fatalf("%s meta omitted explicit empty provider_version: %s", name, data)
		}
	}

	started := ProviderStartedRecord{
		SchemaVersion: SchemaVersion, RootID: strings.Repeat("1", 32), TaskID: strings.Repeat("2", 32),
		SpecSHA256: ComputeSHA256([]byte("spec")), MetaSHA256: ComputeSHA256([]byte("meta")),
		StartedAt: "2026-09-13T00:00:00Z", ProviderExecutable: "/provider",
	}
	data := assertCanonicalRoundTrip(t, started, func(r *ProviderStartedRecord) error { return ValidateProviderStartedRecord(r) })
	if bytes.Contains(data, []byte("provider_version")) || bytes.Contains(data, []byte("provider_sha256")) {
		t.Fatalf("path-only start record encoded absent observations: %s", data)
	}
	populated := started
	populated.ProviderVersion = "provider-v1"
	populated.ProviderSHA256 = ComputeSHA256([]byte("provider"))
	assertCanonicalRoundTrip(t, populated, func(r *ProviderStartedRecord) error { return ValidateProviderStartedRecord(r) })
	historical := started
	historical.ProviderExecutable = ""
	assertCanonicalRoundTrip(t, historical, func(r *ProviderStartedRecord) error { return ValidateProviderStartedRecord(r) })

	seal := *fixtureSeal(t, []byte("stdout"))
	seal.ProviderExecutable = "/provider"
	data = assertCanonicalRoundTrip(t, seal, func(r *ProviderExitRecord) error { return ValidateProviderExitRecord(r) })
	if bytes.Contains(data, []byte("provider_version")) || bytes.Contains(data, []byte("provider_sha256")) {
		t.Fatalf("path-only seal encoded absent observations: %s", data)
	}
	seal.ProviderSHA256 = "invalid"
	if err := ValidateProviderExitRecord(&seal); err == nil {
		t.Fatal("seal with malformed present provider digest was accepted")
	}
}

func assertCanonicalRoundTrip[T any](t *testing.T, record T, validate func(*T) error) []byte {
	t.Helper()
	if err := validate(&record); err != nil {
		t.Fatalf("record rejected: %v", err)
	}
	data, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded T
	if err = DecodeStrict(data, &decoded); err != nil {
		t.Fatalf("canonical record did not decode: %v", err)
	}
	if err = validate(&decoded); err != nil {
		t.Fatalf("decoded record rejected: %v", err)
	}
	again, err := MarshalCanonical(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatalf("canonical bytes changed after round trip:\n%s\n%s", data, again)
	}
	return data
}
