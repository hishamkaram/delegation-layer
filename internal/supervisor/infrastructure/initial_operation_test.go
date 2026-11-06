package infrastructure

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func initialOperationFixtures(t *testing.T, rootID string) []InitialOperation {
	t.Helper()
	operations := []InitialOperation{{}, {Kind: InitialEnsureOwn}, {Kind: InitialMigrateOwn}}
	for _, kind := range []SavedBindingKind{SavedBindingTaskMetadata, SavedBindingTaskSubmission, SavedBindingTaskStop, SavedBindingWorkspaceRequest} {
		source, _ := savedBindingSourceFixture(t, kind)
		source.RootID = rootID
		operations = append(operations, InitialOperation{Kind: InitialRecoverSaved, Source: source})
		if kind == SavedBindingTaskMetadata {
			operations = append(operations, InitialOperation{Kind: InitialPrepareContinuation, Source: source})
		}
	}
	return operations
}

func TestInitialOperationClosedSubjects(t *testing.T) {
	rootID := originTestRootID
	for _, operation := range initialOperationFixtures(t, rootID) {
		if err := operation.Validate(rootID); err != nil {
			t.Fatalf("valid operation refused: %+v: %v", operation, err)
		}
	}
	source, _ := savedBindingSourceFixture(t, SavedBindingTaskMetadata)
	source.RootID = rootID
	for _, operation := range []InitialOperation{
		{Kind: "unknown"},
		{Source: source},
		{Kind: InitialEnsureOwn, Source: source},
		{Kind: InitialRecoverSaved},
		{Kind: InitialPrepareContinuation},
	} {
		if err := operation.Validate(rootID); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid union accepted: %+v: %v", operation, err)
		}
	}
	for _, operation := range initialOperationFixtures(t, rootID)[3:] {
		if err := operation.Validate(strings.Repeat("f", 32)); !errors.Is(err, ErrIdentity) {
			t.Fatalf("wrong target accepted: %+v: %v", operation, err)
		}
	}
}

func TestInitialOperationStrictJSON(t *testing.T) {
	for _, data := range []string{
		`null`, `{}`, `{"kind":""}`, `{"kind":"unknown"}`, `{"kind":null}`,
		`{"kind":"migrate-own","extra":true}`, `{"kind":"migrate-own","source":null}`,
		`{"kind":"migrate-own","source":{}}`, `{"kind":"migrate-referenced","migration":{"root_id":"bad"}}`,
		`{"kind":"migrate-referenced"}`, `{"kind":"migrate-own","migration":{}}`,
	} {
		var operation InitialOperation
		if err := task.DecodeStrict([]byte(data), &operation); err == nil {
			t.Fatalf("invalid descriptor decoded: %s", data)
		}
	}
	for _, operation := range initialOperationFixtures(t, originTestRootID)[1:] {
		data, err := task.MarshalCanonical(operation)
		if err != nil {
			t.Fatal(err)
		}
		var decoded InitialOperation
		if err = task.DecodeStrict(data, &decoded); err != nil || decoded != operation {
			t.Fatalf("descriptor round trip: %s: %v", data, err)
		}
		if operation.Source == (SavedBindingSource{}) && bytes.Contains(data, []byte(`"source"`)) ||
			bytes.Contains(data, []byte(`"migration"`)) {
			t.Fatalf("zero subject was serialized: %s", data)
		}
	}
}

func TestStartupInitialOperationProtocol(t *testing.T) {
	for _, operation := range initialOperationFixtures(t, originTestRootID) {
		for _, protocol := range []int{1, 2, RecordProtocolVersion, RecordProtocolVersion + 1} {
			intent := startupIntentFixture(t)
			intent.Inputs.Initial, intent.Protocol = operation, protocol
			digest, err := intent.Inputs.Digest()
			if err != nil {
				t.Fatal(err)
			}
			intent.InputsSHA256 = digest
			wantValid := protocol == RecordProtocolVersion
			if err = intent.Validate(); (err == nil) != wantValid {
				t.Fatalf("protocol=%d operation=%+v valid=%t: %v", protocol, operation, wantValid, err)
			}
		}
	}
}

// This historical format is independent of the current installed-role type.
// Its executable digests are recorded fixture data, never observations of programs.
type legacyStartupArtifacts struct {
	Runner       string `json:"runner"`
	RunnerSHA256 string `json:"runner_sha256"`
	Client       string `json:"client"`
	ClientSHA256 string `json:"client_sha256"`
	Daemon       string `json:"daemon"`
	DaemonSHA256 string `json:"daemon_sha256"`
}

type legacyStartupInputs struct {
	Kind       StartupKind            `json:"kind"`
	Root       string                 `json:"root"`
	RootID     string                 `json:"root_id"`
	BootID     string                 `json:"boot_id"`
	RootDevice uint64                 `json:"root_device"`
	RootInode  uint64                 `json:"root_inode"`
	Artifacts  legacyStartupArtifacts `json:"artifacts"`
	Context    LaunchContextReference `json:"launch_context"`
}

func TestStartupHistoricalExecutableRecordsRefuseWithoutReplay(t *testing.T) {
	f := producerFixture(t)
	intent := journalIntent(t, f.journal, 1)
	input := intent.Inputs
	legacy := legacyStartupInputs{
		Kind: input.Kind, Root: input.Root, RootID: input.RootID, BootID: input.BootID, RootDevice: input.RootDevice, RootInode: input.RootInode,
		Artifacts: legacyStartupArtifacts{Runner: input.Artifacts.Runner, RunnerSHA256: strings.Repeat("a", 64), Client: filepath.Join(input.Root, "pueue"), ClientSHA256: strings.Repeat("b", 64), Daemon: filepath.Join(input.Root, "pueued"), DaemonSHA256: strings.Repeat("c", 64)}, Context: input.Context,
	}
	inputs, err := task.MarshalCanonical(legacy)
	if err != nil {
		t.Fatal(err)
	}
	historical := struct {
		Protocol     int             `json:"protocol"`
		Sequence     uint64          `json:"sequence"`
		Epoch        string          `json:"epoch"`
		Inputs       json.RawMessage `json:"inputs"`
		InputsSHA256 string          `json:"inputs_sha256"`
		Starter      json.RawMessage `json:"starter"`
		AcceptedAt   string          `json:"accepted_at"`
		Deadline     string          `json:"deadline"`
	}{1, intent.Sequence, intent.Epoch, bytes.TrimSpace(inputs), task.ComputeSHA256(inputs), json.RawMessage(`{"kind":"outside","root":"/old-root","root_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pid":7,"start_token":"fixture:7"}`), intent.AcceptedAt, intent.Deadline}
	data, err := task.MarshalCanonical(historical)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.journal.pin.Root(), "infrastructure", intentName(1))
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	releaseProducerFixtureLease(t, f)
	starts := 0
	attempt, err := producerInvocation(t).establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected native Start") }})
	ownProducerAttempt(t, attempt)
	if err == nil || task.RecordAbsent(err) || starts != 0 || f.host.calls != 0 {
		t.Fatalf("unreadable accepted history authorized replay: starts=%d prepares=%d error=%v", starts, f.host.calls, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, data) {
		t.Fatalf("historical refusal changed accepted bytes: %v", readErr)
	}
}
