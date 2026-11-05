package infrastructure

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func initialOperationFixtures(t *testing.T, rootID string) []InitialOperation {
	t.Helper()
	operations := []InitialOperation{{}, {Kind: InitialEnsureOwn}, {Kind: InitialMigrateOwn}, {
		Kind:      InitialMigrateReferenced,
		Migration: MigrationReference{RootID: rootID, RecordSHA256: strings.Repeat("e", 64), HistoryIndex: 3},
	}}
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
	migration := MigrationReference{RootID: rootID, RecordSHA256: strings.Repeat("e", 64)}
	for _, operation := range []InitialOperation{
		{Kind: "unknown"},
		{Source: source},
		{Kind: InitialEnsureOwn, Source: source},
		{Kind: InitialMigrateOwn, Migration: migration},
		{Kind: InitialRecoverSaved},
		{Kind: InitialPrepareContinuation},
		{Kind: InitialPrepareContinuation, Source: source, Migration: migration},
		{Kind: InitialMigrateReferenced},
		{Kind: InitialRecoverSaved, Source: source, Migration: migration},
		{Kind: InitialMigrateReferenced, Source: source, Migration: migration},
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
			operation.Migration == (MigrationReference{}) && bytes.Contains(data, []byte(`"migration"`)) {
			t.Fatalf("zero subject was serialized: %s", data)
		}
	}
}

func TestStartupInitialOperationProtocol(t *testing.T) {
	for _, operation := range initialOperationFixtures(t, originTestRootID) {
		for _, protocol := range []int{ProtocolVersion, startupInitialOperationProtocolVersion, 3} {
			intent := startupIntentFixture(t)
			intent.Inputs.Initial, intent.Protocol = operation, protocol
			digest, err := intent.Inputs.Digest()
			if err != nil {
				t.Fatal(err)
			}
			intent.InputsSHA256 = digest
			wantValid := protocol == ProtocolVersion && operation == (InitialOperation{}) ||
				protocol == startupInitialOperationProtocolVersion && operation != (InitialOperation{}) && operation.Kind != InitialEnsureOwn
			if err = intent.Validate(); (err == nil) != wantValid {
				t.Fatalf("protocol=%d operation=%+v valid=%t: %v", protocol, operation, wantValid, err)
			}
		}
	}
	inputs := startupIntentFixture(t).Inputs
	inputs.Kind, inputs.Artifacts, inputs.Context = StartupUpgrade, Artifacts{}, LaunchContextReference{}
	inputs.UpgradeID, inputs.UpgradeSHA256 = originTestRootID, strings.Repeat("a", 64)
	if err := inputs.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range initialOperationFixtures(t, originTestRootID)[1:] {
		inputs.Initial = operation
		if err := inputs.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("upgrade accepted initial operation: %v", err)
		}
	}
}

// The legacy schema is deliberately independent of StartupInputs, including
// its field order and tags. Native fixture paths are encoded by encoding/json.
type legacyStartupInputs struct {
	Kind          StartupKind            `json:"kind"`
	Root          string                 `json:"root"`
	RootID        string                 `json:"root_id"`
	BootID        string                 `json:"boot_id"`
	RootDevice    uint64                 `json:"root_device"`
	RootInode     uint64                 `json:"root_inode"`
	Artifacts     Artifacts              `json:"artifacts"`
	Context       LaunchContextReference `json:"launch_context"`
	UpgradeID     string                 `json:"upgrade_id,omitempty"`
	UpgradeSHA256 string                 `json:"upgrade_sha256,omitempty"`
}

func TestStartupLegacyInitialOperationBytesAndDigest(t *testing.T) {
	intent := startupIntentFixture(t)
	i := intent.Inputs
	legacy := legacyStartupInputs{i.Kind, i.Root, i.RootID, i.BootID, i.RootDevice, i.RootInode, i.Artifacts, i.Context, "", ""}
	expected, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	expected = append(expected, '\n')
	actual, err := task.MarshalCanonical(i)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("legacy inputs bytes changed: %s: %v", actual, err)
	}
	if i.Initial != (InitialOperation{}) || intent.InputsSHA256 != task.ComputeSHA256(expected) {
		t.Fatal("legacy Own input digest changed")
	}
	var decoded StartupInputs
	if err = task.DecodeStrict(expected, &decoded); err != nil || decoded != i {
		t.Fatalf("legacy inputs without initial descriptor: %v", err)
	}
	// Independently replace only the inputs with the verified legacy bytes.
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	var legacyIntent struct {
		Protocol     int               `json:"protocol"`
		Sequence     uint64            `json:"sequence"`
		Epoch        string            `json:"epoch"`
		Inputs       json.RawMessage   `json:"inputs"`
		InputsSHA256 string            `json:"inputs_sha256"`
		Starter      StartupProvenance `json:"starter"`
		AcceptedAt   string            `json:"accepted_at"`
		Deadline     string            `json:"deadline"`
	}
	legacyIntent.Protocol, legacyIntent.Sequence, legacyIntent.Epoch = 1, intent.Sequence, intent.Epoch
	legacyIntent.Inputs, legacyIntent.InputsSHA256 = bytes.TrimSpace(expected), task.ComputeSHA256(expected)
	legacyIntent.Starter, legacyIntent.AcceptedAt, legacyIntent.Deadline = intent.Starter, intent.AcceptedAt, intent.Deadline
	expectedIntent, err := task.MarshalCanonical(legacyIntent)
	if err != nil || !bytes.Equal(data, expectedIntent) {
		t.Fatalf("legacy intent bytes changed: %v", err)
	}
	digest, err := intent.Digest()
	if err != nil || digest != task.ComputeSHA256(expectedIntent) {
		t.Fatalf("legacy intent digest changed: %v", err)
	}
}
