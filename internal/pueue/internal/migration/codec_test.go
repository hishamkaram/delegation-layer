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
