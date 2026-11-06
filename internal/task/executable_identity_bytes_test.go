package task

import (
	"fmt"
	"strings"
	"testing"
)

func TestLegacySupervisorIdentityCanonicalBytesRemainStable(t *testing.T) {
	ref := legacySupervisorFixture()
	want := fmt.Sprintf(`{"client_executable":"/opt/tools/pueue","client_sha256":"%s","daemon_executable":"/opt/tools/pueued","daemon_sha256":"%s","resolved_config_sha256":"%s","endpoint":"unix:/state/.supervisor/socket","config_path":"/state/.supervisor/pueue.yml","config_digest":"%s","observed_version":"4.0.4"}`+"\n", strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("d", 64), strings.Repeat("c", 64))
	requireIdentityCanonicalBytes(t, ref, want, "12568c3ffff8edaeb78bb5f89b8109737c363aab1ed201f02dfa3a9e170de7ba")
	receipt := executableReceiptFixture(ref)
	want = fmt.Sprintf(`{"meta_sha256":"%s","config_path":"/state/.supervisor/pueue.yml","client_executable":"/opt/tools/pueue","client_sha256":"%s","daemon_executable":"/opt/tools/pueued","daemon_sha256":"%s","resolved_config_sha256":"%s","schema_version":1,"root_id":"%s","task_id":"%s","spec_sha256":"%s","numeric_task_id":7,"label":"delegate:%s:%s","config_digest":"%s","endpoint":"unix:/state/.supervisor/socket","observed_version":"4.0.4"}`+"\n", strings.Repeat("f", 64), strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("d", 64), strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("e", 64), strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("c", 64))
	requireIdentityCanonicalBytes(t, receipt, want, "3e52cd22b377eaa748f5fa8f71596fbfad94050ab11df87baab234710f4e6d1a")
}

func requireIdentityCanonicalBytes[T SupervisorRef | SupervisorReceipt](t *testing.T, value T, want, digest string) {
	t.Helper()
	data, err := MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want || ComputeSHA256(data) != digest {
		t.Fatalf("historical canonical record changed: %s", data)
	}
	var decoded T
	if err = DecodeStrict(data, &decoded); err != nil || decoded != value {
		t.Fatalf("historical record round trip failed: %v", err)
	}
}

func TestInstalledSupervisorIdentityCanonicalOrderingAndOmissions(t *testing.T) {
	ref := installedSupervisorFixture()
	data, err := MarshalCanonical(ref)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"client_executable":"/opt/tools/pueue","daemon_executable":"/opt/tools/pueued","executable_identity":"installed-path-v1","resolved_config_sha256":"%s","endpoint":"unix:/state/.supervisor/socket","config_path":"/state/.supervisor/pueue.yml","config_digest":"%s","observed_version":"4.0.4"}`+"\n", strings.Repeat("d", 64), strings.Repeat("c", 64))
	if string(data) != want {
		t.Fatalf("installed canonical representation changed: %s", data)
	}
	data, err = MarshalCanonical(executableReceiptFixture(ref))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"client_sha256"`, `"daemon_sha256"`} {
		if strings.Contains(string(data), key) {
			t.Fatalf("marked receipt contains an executable digest: %s", data)
		}
	}
	if !strings.Contains(string(data), `"daemon_executable":"/opt/tools/pueued","executable_identity":"installed-path-v1","resolved_config_sha256"`) {
		t.Fatalf("receipt marker order changed: %s", data)
	}
}
