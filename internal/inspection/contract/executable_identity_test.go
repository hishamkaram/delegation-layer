package contract

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func executableBindingFixture() Binding {
	return Binding{
		DefinitionRevision: "fixture-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/installed/helper", WorkerExecutable: "/installed/worker", WorkerSHA256: strings.Repeat("b", 64),
		Supervisor: task.SupervisorRef{
			ClientExecutable: "/installed/pueue", ClientSHA256: strings.Repeat("c", 64),
			ResolvedConfigSHA256: strings.Repeat("d", 64), Endpoint: "unix:/state/socket", ConfigPath: "/state/pueue.yml", ConfigDigest: strings.Repeat("e", 64), ObservedVersion: "pueue 4.0.4",
		},
	}
}

func TestInspectionBindingInstalledExecutableIdentity(t *testing.T) {
	marked := executableBindingFixture()
	marked.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	marked.WorkerSHA256 = ""
	if err := ValidateBinding(marked); err != nil {
		t.Fatalf("installed binding requires executable digest: %v", err)
	}
	data, err := task.MarshalCanonical(marked)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"helper_sha256"`)) || bytes.Contains(data, []byte(`"worker_sha256"`)) {
		t.Fatalf("fresh installed binding emitted executable digest fields: %s", data)
	}
	var roundTrip Binding
	if err = task.DecodeStrict(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.ExecutableIdentity != marked.ExecutableIdentity || roundTrip.HelperExecutable != marked.HelperExecutable || roundTrip.WorkerExecutable != marked.WorkerExecutable {
		t.Fatal("installed binding projection changed")
	}
}

func TestInspectionBindingIdentityRejectsUnknownMixedAndTruncatedLegacy(t *testing.T) {
	for _, kind := range []string{"unknown", "mixed helper", "mixed worker", "truncated legacy"} {
		t.Run(kind, func(t *testing.T) {
			binding := executableBindingFixture()
			switch kind {
			case "unknown":
				binding.ExecutableIdentity = "unknown"
			case "mixed helper":
				binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
				binding.WorkerSHA256 = ""
				binding.HelperSHA256 = strings.Repeat("f", 64)
			case "mixed worker":
				binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
			case "truncated legacy":
				binding.WorkerSHA256 = ""
			}
			if err := ValidateBinding(binding); err == nil {
				t.Fatal("invalid executable identity accepted")
			}
		})
	}
}

func TestInspectionBindingLegacyEmptyHelperBytesStayCanonical(t *testing.T) {
	binding := executableBindingFixture()
	want := `{"definition_revision":"fixture-v1","definition_sha256":"` + strings.Repeat("a", 64) + `","helper_executable":"/installed/helper","helper_sha256":"","worker_executable":"/installed/worker","worker_sha256":"` + strings.Repeat("b", 64) + `","supervisor":{"client_executable":"/installed/pueue","client_sha256":"` + strings.Repeat("c", 64) + `","resolved_config_sha256":"` + strings.Repeat("d", 64) + `","endpoint":"unix:/state/socket","config_path":"/state/pueue.yml","config_digest":"` + strings.Repeat("e", 64) + `","observed_version":"pueue 4.0.4"}}` + "\n"
	if err := ValidateBinding(binding); err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(binding)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("legacy canonical bytes changed: %s", data)
	}
	var decoded Binding
	if err = task.DecodeStrict(data, &decoded); err != nil {
		t.Fatal(err)
	}
	again, err := task.MarshalCanonical(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("legacy request digest input changed on replay")
	}
}

func TestInstalledInspectionBindingWireEmptyDigestSemantics(t *testing.T) {
	marked := executableBindingFixture()
	marked.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	marked.WorkerSHA256 = ""
	canonical, err := task.MarshalCanonical(marked)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"helper_sha256", "worker_sha256"} {
		encoded := append([]byte(`{"`+field+`":"",`), canonical[1:]...)
		var decoded Binding
		if err = task.DecodeStrict(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if err = ValidateBinding(decoded); err != nil {
			t.Fatalf("explicit empty %s rejected: %v", field, err)
		}
		again, marshalErr := task.MarshalCanonical(decoded)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if !bytes.Equal(canonical, again) {
			t.Fatal("empty digest presence changed canonical installed representation")
		}
		nullField := append([]byte(`{"`+field+`":null,`), canonical[1:]...)
		if err = task.DecodeStrict(nullField, &decoded); err == nil {
			t.Fatalf("null %s accepted", field)
		}
	}
}

func TestLegacyInspectionBindingRequiresHistoricalDigestFields(t *testing.T) {
	legacy := executableBindingFixture()
	encoded, err := task.MarshalCanonical(legacy)
	if err != nil {
		t.Fatal(err)
	}
	missing := bytes.Replace(encoded, []byte(`"helper_sha256":"",`), nil, 1)
	var decoded Binding
	if err = task.DecodeStrict(missing, &decoded); err == nil {
		t.Fatal("missing required legacy helper digest field accepted")
	}
	withNull := bytes.Replace(encoded, []byte(`"helper_sha256":""`), []byte(`"helper_sha256":null`), 1)
	if err = task.DecodeStrict(withNull, &decoded); err == nil {
		t.Fatal("legacy null helper digest accepted")
	}
}
