package control

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These first-supporting-build bytes are the permanent drain-v1 compatibility
// fixtures. Read them in future builds; never regenerate them per release.
func TestUpgradeDrainV1FrozenPhaseBytes(t *testing.T) {
	for _, phase := range []UpgradePhase{UpgradeDraining, UpgradeAwaitingInstallation, UpgradeAuthorized, UpgradeMigrating} {
		t.Run(string(phase), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "upgrade-v1", "drain-"+string(phase)+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var record privateBootstrapRecord
			if err = task.DecodeStrict(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.DrainV1 == nil || record.DrainV1.Phase != phase || record.DrainV1.OperationID != UpgradeOperationID("upgrade-v1:"+strings.Repeat("2", 32)) {
				t.Fatal("frozen phase decoded a different subject")
			}
			if err = validatePrivateUpgradeDrain("/state", *record.DrainV1); err != nil {
				t.Fatal(err)
			}
			encoded, err := task.MarshalCanonical(record)
			if err != nil || !bytes.Equal(encoded, data) {
				t.Fatalf("frozen drain-v1 encoding changed: err=%v", err)
			}
		})
	}
}

func TestUpgradeMigrationFrozenProgressBytes(t *testing.T) {
	for index, phase := range []string{"prepared", "shutdown_requested", "shutdown_dispatched", "predecessor_closed", "accepted_start", "birth_recorded", "completed"} {
		t.Run(phase, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "upgrade-v1", "migration-"+phase+".json"))
			if err != nil {
				t.Fatal(err)
			}
			record, invalid := migration.DecodeRecord(data)
			if invalid != nil {
				t.Fatal(invalid)
			}
			if record.Legacy != record.Successor || record.UpgradeGeneration.OperationID == "" || record.LegacyStopped != (index >= 3) || record.UpgradeGeneration.AcceptedStart != (index >= 4) || record.UpgradeGeneration.Completed != (index >= 6) {
				t.Fatal("frozen progress decoded a different transition")
			}
			encoded, invalid := migration.EncodeRecord(record)
			if invalid != nil || !bytes.Equal(encoded, data) {
				t.Fatalf("frozen upgrade migration encoding changed: invalid=%v", invalid)
			}
			ordinary := record
			ordinary.UpgradeGeneration = migration.UpgradeGeneration{}
			if _, invalid = migration.EncodeRecord(ordinary); invalid == nil || invalid.Code != migration.ViolationIdenticalIdentity {
				t.Fatal("ordinary migration accepted equal identities")
			}
		})
	}
}

func TestUpgradeMigrationHistoryRejectsDuplicateOperationWinners(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "upgrade-v1", "migration-completed.json"))
	if err != nil {
		t.Fatal(err)
	}
	record, invalid := migration.DecodeRecord(data)
	if invalid != nil {
		t.Fatal(invalid)
	}
	if _, invalid = migration.EncodeHistory(migration.History{SchemaVersion: task.SchemaVersion, Migrations: []migration.Record{record, record}}); invalid == nil {
		t.Fatal("history admitted two winners for the same operation")
	}
	record.UpgradeGeneration.Completed = false
	if _, invalid = migration.EncodeHistory(migration.History{SchemaVersion: task.SchemaVersion, Migrations: []migration.Record{record}}); invalid == nil {
		t.Fatal("history admitted unverified upgrade completion")
	}
}

func TestUpgradeFreshBirthUsesCanonicalBootIdentity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "upgrade-v1", "migration-completed.json"))
	if err != nil {
		t.Fatal(err)
	}
	record, invalid := migration.DecodeRecord(data)
	if invalid != nil {
		t.Fatal(invalid)
	}
	record.UpgradeGeneration.LegacyBootID = "linux:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	record.UpgradeGeneration.SuccessorBootID = "linux:AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"
	record.UpgradeGeneration.SuccessorPID = record.LegacyPID
	record.UpgradeGeneration.SuccessorProcessStart = record.LegacyProcessStart
	if _, invalid = migration.EncodeRecord(record); invalid == nil {
		t.Fatal("canonical same-boot predecessor was accepted as a fresh birth")
	}
}
