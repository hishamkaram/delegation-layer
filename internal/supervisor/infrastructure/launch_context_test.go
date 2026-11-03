package infrastructure

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func launchContextFixture(t *testing.T) LaunchContext {
	t.Helper()
	origin := testOrigin(t)
	return LaunchContext{
		Protocol: ProtocolVersion, Root: origin.Root(), RootID: origin.RootID(), OS: "linux",
		ControlEnvironment:     []string{"HOME=/selected", "CALLBACK_CONTEXT=preserved"},
		MaintenanceEnvironment: []string{},
	}
}

func TestInfrastructureLaunchContextBindsExactExplicitSnapshots(t *testing.T) {
	record := launchContextFixture(t)
	digest, err := record.Digest()
	if err != nil {
		t.Fatal(err)
	}
	reference := LaunchContextReference{SHA256: digest}
	if err = reference.ValidateContext(record, record.Root, record.RootID); err != nil {
		t.Fatal(err)
	}
	changed := record.Clone()
	changed.ControlEnvironment[1] = "CALLBACK_CONTEXT=different"
	if err = reference.ValidateContext(changed, record.Root, record.RootID); !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed callback context accepted: %v", err)
	}
	if !slices.Equal(record.ControlEnvironment, []string{"HOME=/selected", "CALLBACK_CONTEXT=preserved"}) {
		t.Fatal("cloning exposed the original environment")
	}
	if err = reference.ValidateContext(record, record.Root, strings.Repeat("e", 32)); !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign context namespace accepted: %v", err)
	}
	manifest, chunks, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := task.MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var restoredManifest contextManifest
	if err = task.DecodeStrict(encoded, &restoredManifest); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	for _, chunk := range chunks {
		payload = append(payload, chunk.Data...)
	}
	restored, err := decodeContextPayload(restoredManifest, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = reference.ValidateContext(restored, record.Root, record.RootID); err != nil || restored.MaintenanceEnvironment == nil {
		t.Fatalf("explicit empty snapshot lost in storage: %v", err)
	}
}

func TestInfrastructureLaunchContextRefusesInvalidEnvironmentWithoutValues(t *testing.T) {
	for _, environment := range [][]string{nil, {"KEY=private", "KEY=other"}, {"KEY=private\x00"}} {
		record := launchContextFixture(t)
		record.ControlEnvironment = environment
		_, err := record.Digest()
		if !errors.Is(err, ErrIdentity) || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid context or values exposed: %v", err)
		}
	}
}
