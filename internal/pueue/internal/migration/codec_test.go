package migration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecordCodecPreservesCanonicalBytes(t *testing.T) {
	record := migrationTestRecord()
	got, invalid := EncodeRecord(record)
	if invalid != nil {
		t.Fatal(invalid)
	}
	want := fmt.Sprintf(
		`{"schema_version":1,"legacy":{"executable":"/usr/bin/pueued","sha256":"%s","config_path":"/state/.supervisor/pueue.yml","config_digest":"%s","resolved_config_sha256":"%s","endpoint":"unix:/state/.supervisor/run/pueue.sock"},"successor":{"executable":"/opt/pueued","sha256":"%s","config_path":"/state/.supervisor/pueue.yml","config_digest":"%s","resolved_config_sha256":"%s","endpoint":"unix:/state/.supervisor/run/pueue.sock"},"legacy_pid":4123,"legacy_process_start":"pid-start-4123","legacy_shutdown_requested":true,"legacy_shutdown_dispatched":true,"legacy_stopped":true}`+"\n",
		strings.Repeat("a", 64), strings.Repeat("c", 64), strings.Repeat("d", 64),
		strings.Repeat("b", 64), strings.Repeat("e", 64), strings.Repeat("f", 64),
	)
	if string(got) != want {
		t.Fatalf("migration record bytes changed:\n got: %q\nwant: %q", got, want)
	}
	if task.ComputeSHA256(got) != "ebc5cc7ed7ab6f0757fbd0845d940287c47710538026426895f41e4f404237d6" {
		t.Fatal("legacy migration record digest changed")
	}
	history, invalid := EncodeHistory(History{SchemaVersion: task.SchemaVersion, Migrations: []Record{record}})
	if invalid != nil {
		t.Fatal(invalid)
	}
	expectedHistory := `{"schema_version":1,"migrations":[` + strings.TrimSuffix(want, "\n") + "]}\n"
	if string(history) != expectedHistory || task.ComputeSHA256(history) != "1a4bafef1f14aa43b2c2f3f99c0678c466bedc3779b0aa0de09f20fa32ebc2dd" {
		t.Fatal("legacy history bytes or digest changed")
	}
	decoded, invalid := DecodeRecord(got)
	if invalid != nil || decoded != record {
		t.Fatalf("canonical migration record did not round trip: record=%+v invalid=%v", decoded, invalid)
	}
}

func TestRecordCodecReturnsTypedViolations(t *testing.T) {
	record := migrationTestRecord()
	identical := record
	identical.Successor = identical.Legacy
	if _, invalid := EncodeRecord(identical); invalid == nil || invalid.Code != ViolationIdenticalIdentity {
		t.Fatalf("identical identities violation = %+v", invalid)
	}
	invalidProcess := record
	invalidProcess.LegacyPID = -1
	if _, invalid := EncodeRecord(invalidProcess); invalid == nil || invalid.Code != ViolationProcessState {
		t.Fatalf("invalid process-state violation = %+v", invalid)
	}
	unsupported := record
	unsupported.SchemaVersion++
	unsupportedData, err := task.MarshalCanonical(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	if _, invalid := DecodeRecord(unsupportedData); invalid == nil || invalid.Code != ViolationRecordSchema || invalid.SchemaVersion != unsupported.SchemaVersion {
		t.Fatalf("unsupported schema violation = %+v", invalid)
	}
	if _, invalid := DecodeRecord([]byte(`{"schema_version":`)); invalid == nil || invalid.Code != ViolationRecordDecode || invalid.Cause == nil {
		t.Fatalf("malformed record violation = %+v", invalid)
	}
}

func TestHistoryCodecRequiresStoppedPredecessors(t *testing.T) {
	record := migrationTestRecord()
	history := History{SchemaVersion: task.SchemaVersion, Migrations: []Record{record}}
	data, invalid := EncodeHistory(history)
	if invalid != nil {
		t.Fatal(invalid)
	}
	decoded, invalid := DecodeHistory(data)
	if invalid != nil || len(decoded.Migrations) != 1 || decoded.Migrations[0] != record {
		t.Fatalf("migration history did not round trip: history=%+v invalid=%v", decoded, invalid)
	}
	active := record
	active.LegacyStopped = false
	if _, invalid = EncodeHistory(History{SchemaVersion: task.SchemaVersion, Migrations: []Record{active}}); invalid == nil || invalid.Code != ViolationHistoryUnproven {
		t.Fatalf("active predecessor history violation = %+v", invalid)
	}
}

func TestShutdownAcknowledgmentRequiresRequestDispatchAndPredecessorBirth(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Record)
	}{
		{"request", func(record *Record) { record.LegacyShutdownRequested = false }},
		{"dispatch", func(record *Record) { record.LegacyShutdownDispatched = false }},
		{"pid", func(record *Record) { record.LegacyPID = 0 }},
		{"start", func(record *Record) { record.LegacyProcessStart = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := migrationTestRecord()
			record.LegacyShutdownAcknowledged = true
			test.mutate(&record)
			if _, invalid := EncodeRecord(record); invalid == nil || invalid.Code != ViolationProcessState {
				t.Fatalf("invalid acknowledged shutdown was encoded: %v", invalid)
			}
			data, err := task.MarshalCanonical(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, invalid := DecodeRecord(data); invalid == nil || invalid.Code != ViolationProcessState {
				t.Fatalf("invalid acknowledged shutdown was decoded: %v", invalid)
			}
		})
	}
}

func TestShutdownAcknowledgmentRoundTripsAndOlderStrictReadersRefuse(t *testing.T) {
	record := migrationTestRecord()
	legacy, invalid := EncodeRecord(record)
	if invalid != nil {
		t.Fatal(invalid)
	}
	var old legacyShutdownRecord
	if err := task.DecodeStrict(legacy, &old); err != nil {
		t.Fatalf("frozen older shape did not accept its historical record: %v", err)
	}
	record.LegacyShutdownAcknowledged = true
	record.LegacyStopped = false
	data, invalid := EncodeRecord(record)
	if invalid != nil {
		t.Fatal(invalid)
	}
	decoded, invalid := DecodeRecord(data)
	if invalid != nil || decoded != record {
		t.Fatalf("acknowledged pending migration did not round trip: record=%+v invalid=%v", decoded, invalid)
	}
	if err := task.DecodeStrict(data, &old); err == nil || !strings.Contains(err.Error(), `"legacy_shutdown_acknowledged"`) {
		t.Fatalf("older strict record reader did not refuse the populated fact: %v", err)
	}
	record.LegacyStopped = true
	history := History{SchemaVersion: task.SchemaVersion, Migrations: []Record{record}}
	data, invalid = EncodeHistory(history)
	if invalid != nil {
		t.Fatal(invalid)
	}
	decodedHistory, invalid := DecodeHistory(data)
	if invalid != nil || len(decodedHistory.Migrations) != 1 || decodedHistory.Migrations[0] != record {
		t.Fatalf("history lost acknowledged shutdown: history=%+v invalid=%v", decodedHistory, invalid)
	}
	var oldHistory struct {
		SchemaVersion int                    `json:"schema_version"`
		Migrations    []legacyShutdownRecord `json:"migrations"`
	}
	if err := task.DecodeStrict(data, &oldHistory); err == nil || !strings.Contains(err.Error(), `"legacy_shutdown_acknowledged"`) {
		t.Fatalf("older strict history reader did not refuse the populated fact: %v", err)
	}
}

// Frozen pre-acknowledgement JSON shape independently models older strict
// readers. Its acceptance of the legacy fixture is checked before refusal.
type legacyShutdownRecord struct {
	SchemaVersion            int               `json:"schema_version"`
	Legacy                   DaemonIdentity    `json:"legacy"`
	Successor                DaemonIdentity    `json:"successor"`
	LegacyPID                int               `json:"legacy_pid,omitempty"`
	LegacyProcessStart       string            `json:"legacy_process_start,omitempty"`
	LegacyShutdownRequested  bool              `json:"legacy_shutdown_requested,omitempty"`
	LegacyShutdownDispatched bool              `json:"legacy_shutdown_dispatched,omitempty"`
	LegacyStopped            bool              `json:"legacy_stopped,omitempty"`
	UpgradeGeneration        UpgradeGeneration `json:"upgrade_generation,omitzero"`
}

func migrationTestRecord() Record {
	return Record{
		SchemaVersion: task.SchemaVersion,
		Legacy: DaemonIdentity{
			Executable: "/usr/bin/pueued", SHA256: strings.Repeat("a", 64),
			ConfigPath: "/state/.supervisor/pueue.yml", ConfigDigest: strings.Repeat("c", 64),
			ResolvedConfigSHA256: strings.Repeat("d", 64), Endpoint: "unix:/state/.supervisor/run/pueue.sock",
		},
		Successor: DaemonIdentity{
			Executable: "/opt/pueued", SHA256: strings.Repeat("b", 64),
			ConfigPath: "/state/.supervisor/pueue.yml", ConfigDigest: strings.Repeat("e", 64),
			ResolvedConfigSHA256: strings.Repeat("f", 64), Endpoint: "unix:/state/.supervisor/run/pueue.sock",
		},
		LegacyPID: 4123, LegacyProcessStart: "pid-start-4123",
		LegacyShutdownRequested: true, LegacyShutdownDispatched: true, LegacyStopped: true,
	}
}
