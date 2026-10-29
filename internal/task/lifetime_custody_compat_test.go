package task

import (
	"bytes"
	"strings"
	"testing"
)

const (
	legacyLifetimeRegistrationJSON = `{"schema_version":1,"root_id":"11111111111111111111111111111111","task_id":"22222222222222222222222222222222","owner_kind":"task","owner_sha256":"4c1029697ee358715d3a14a2add817c4b01651440de808371f78165ac90dc581","scope_kind":"cgroup-v2","assurance":"exact","scope_id":"/sys/fs/cgroup/delegate-test","boot_id":"boot-token","deadline":"2026-10-11T12:00:00Z","created_at":"2026-10-11T11:00:00Z"`
	legacyLifetimeRegistrationSHA  = "0b5192d89a728965a22efc9b4ac0dc29acd2b3235d338c4ce435b5135920098c"
	custodyPlacementJSON           = `,"custody_placement":{"version":1,"pool_root":"/var/lib/delegate/pool","pool_id":"` +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `"}`
)

// A registration written before custody placement existed keeps its exact
// canonical bytes and digest; the digest is the identity other records bind to.
func TestLifetimeRegistrationWithoutPlacementKeepsLegacyBytesAndDigest(t *testing.T) {
	want := []byte(legacyLifetimeRegistrationJSON + "}\n")
	record := validLifetimeRegistration()
	got, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical legacy registration changed:\n got %s\nwant %s", got, want)
	}
	if digest := ComputeSHA256(got); digest != legacyLifetimeRegistrationSHA {
		t.Fatalf("legacy registration digest = %s, want %s", digest, legacyLifetimeRegistrationSHA)
	}
	var decoded LifetimeRegistrationRecord
	if err := DecodeStrict(want, &decoded); err != nil {
		t.Fatalf("legacy registration rejected: %v", err)
	}
	if err := ValidateLifetimeRegistrationRecord(&decoded); err != nil {
		t.Fatalf("legacy registration invalid: %v", err)
	}
	if again, err := MarshalCanonical(decoded); err != nil || !bytes.Equal(again, want) {
		t.Fatalf("legacy registration did not round-trip: %q, %v", again, err)
	}
}

// A registration carrying custody placement is read and rewritten through the
// current record API without losing or reordering any field.
func TestLifetimeRegistrationRoundTripsPopulatedPlacementFixture(t *testing.T) {
	fixture := []byte(legacyLifetimeRegistrationJSON + custodyPlacementJSON + "}\n")
	var decoded LifetimeRegistrationRecord
	if err := DecodeStrict(fixture, &decoded); err != nil {
		t.Fatalf("registration with custody placement rejected: %v", err)
	}
	if err := ValidateLifetimeRegistrationRecord(&decoded); err != nil {
		t.Fatalf("registration with custody placement invalid: %v", err)
	}
	again, err := MarshalCanonical(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, fixture) {
		t.Fatalf("placement registration did not round-trip:\n got %s\nwant %s", again, fixture)
	}
	if !strings.Contains(string(again), `"custody_placement":{"version":1,"pool_root":`) {
		t.Fatalf("placement member order or names changed: %s", again)
	}
}
