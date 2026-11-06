package migration

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func installedDaemonFixture() DaemonIdentity {
	ref := migrationTestRecord().Successor
	ref.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	ref.SHA256 = ""
	return ref
}

func TestDaemonExecutableIdentityValidation(t *testing.T) {
	if invalid := ValidateDaemonIdentity(installedDaemonFixture()); invalid != nil {
		t.Fatal(invalid)
	}
	for _, test := range []struct {
		name   string
		mutate func(*DaemonIdentity)
	}{
		{"unknown", func(d *DaemonIdentity) { d.ExecutableIdentity = "future-mode" }},
		{"executable digest", func(d *DaemonIdentity) { d.SHA256 = strings.Repeat("a", 64) }},
		{"missing path", func(d *DaemonIdentity) { d.Executable = "" }},
		{"relative path", func(d *DaemonIdentity) { d.Executable = "pueued" }},
		{"unclean path", func(d *DaemonIdentity) { d.Executable = "/opt/../pueued" }},
		{"nul path", func(d *DaemonIdentity) { d.Executable = "/opt/pueued\x00" }},
		{"missing logical identity", func(d *DaemonIdentity) {
			d.ConfigPath, d.ConfigDigest, d.ResolvedConfigSHA256, d.Endpoint = "", "", "", ""
		}},
		{"missing config", func(d *DaemonIdentity) { d.ConfigPath = "" }},
		{"missing digest", func(d *DaemonIdentity) { d.ConfigDigest = "" }},
		{"missing resolved digest", func(d *DaemonIdentity) { d.ResolvedConfigSHA256 = "" }},
		{"missing endpoint", func(d *DaemonIdentity) { d.Endpoint = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := installedDaemonFixture()
			test.mutate(&identity)
			if invalid := ValidateDaemonIdentity(identity); invalid == nil {
				t.Fatal("invalid installed identity accepted")
			}
		})
	}
}

func TestLegacyDaemonIdentityKeepsHistoricalShape(t *testing.T) {
	legacy := DaemonIdentity{Executable: "/usr/bin/pueued", SHA256: strings.Repeat("a", 64)}
	if invalid := ValidateDaemonIdentity(legacy); invalid != nil {
		t.Fatalf("legacy executable-only identity refused: %v", invalid)
	}
	legacy.SHA256 = ""
	if invalid := ValidateDaemonIdentity(legacy); invalid == nil {
		t.Fatal("truncated legacy identity became installed identity")
	}
	legacy.SHA256 = "invalid"
	if invalid := ValidateDaemonIdentity(legacy); invalid == nil {
		t.Fatal("malformed legacy digest accepted")
	}
}

func TestMigrationCodecPreservesBothExecutableIdentityForms(t *testing.T) {
	for _, installedPredecessor := range []bool{false, true} {
		record := migrationTestRecord()
		record.Successor = installedDaemonFixture()
		if installedPredecessor {
			record.Legacy.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
			record.Legacy.SHA256 = ""
		}
		data, invalid := EncodeRecord(record)
		if invalid != nil {
			t.Fatal(invalid)
		}
		decoded, invalid := DecodeRecord(data)
		if invalid != nil || decoded != record {
			t.Fatalf("migration changed identity representation: %+v %v", decoded, invalid)
		}
		history := History{SchemaVersion: task.SchemaVersion, Migrations: []Record{record}}
		data, invalid = EncodeHistory(history)
		if invalid != nil {
			t.Fatal(invalid)
		}
		recovered, invalid := DecodeHistory(data)
		if invalid != nil || len(recovered.Migrations) != 1 || recovered.Migrations[0] != record {
			t.Fatalf("history lost representation: %+v %v", recovered, invalid)
		}
	}
	data, err := task.MarshalCanonical(installedDaemonFixture())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"sha256"`)) || !bytes.Contains(data, []byte(`"executable_identity":"installed-path-v1"`)) {
		t.Fatalf("installed identity retained executable digest: %s", data)
	}
}

func TestDaemonExecutableIdentityViolationsStayTyped(t *testing.T) {
	for _, marker := range []task.ExecutableIdentity{"unknown", task.ExecutableIdentityInstalledPathV1} {
		identity := migrationTestRecord().Successor
		identity.ExecutableIdentity = marker
		if invalid := ValidateDaemonIdentity(identity); invalid == nil || invalid.Code != ViolationExecutableIdentity {
			t.Fatalf("identity violation lost its classification: %v", invalid)
		}
	}
}
