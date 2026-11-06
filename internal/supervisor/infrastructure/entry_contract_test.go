package infrastructure

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func rootOperationFixture(t *testing.T) (StartupIntent, LaunchContext) {
	t.Helper()
	intent := startupIntentFixture(t)
	launchContext := LaunchContext{Protocol: RecordProtocolVersion, Root: intent.Inputs.Root, RootID: intent.Inputs.RootID, OS: runtime.GOOS, ControlEnvironment: []string{"PROVIDER=selected"}, MaintenanceEnvironment: []string{}, SupervisorExecutable: filepath.Join(intent.Inputs.Root, "pueue")}
	digest, err := launchContext.Digest()
	if err != nil {
		t.Fatal(err)
	}
	intent.Inputs.Context = LaunchContextReference{SHA256: digest}
	refreshIntentDigest(t, &intent)
	return intent, launchContext
}

func TestRootOwnerOperationOwnsIntentAndDerivesArtifacts(t *testing.T) {
	intent, launchContext := rootOperationFixture(t)
	operation, err := newRootOwnerOperation(intent, launchContext)
	if err != nil {
		t.Fatal(err)
	}
	want := intent
	intent.Inputs.Context = LaunchContextReference{}
	launchContext.ControlEnvironment[0] = "changed"
	if operation.Intent() != want || operation.Artifacts() != want.Inputs.Artifacts || operation.defaultContextReference() != want.Inputs.Context {
		t.Fatal("operation changed source records or aliases constructor input")
	}
	if err = operation.Validate(); err != nil {
		t.Fatalf("constructor input changed immutable operation: %v", err)
	}
}

func TestRootOwnerOperationRefusesZeroForeignAndUnsupportedKind(t *testing.T) {
	if err := (Operation{}).Validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("zero operation accepted: %v", err)
	}
	intent, launchContext := rootOperationFixture(t)
	for _, kind := range []string{"root", "root-id", "context", "malformed-context", "os", "intent", "upgrade"} {
		changedIntent, changedContext := intent, launchContext.Clone()
		switch kind {
		case "root":
			changedContext.Root = "/different"
		case "root-id":
			changedContext.RootID = strings.Repeat("e", 32)
		case "context":
			changedContext.ControlEnvironment[0] = "different=value"
		case "malformed-context":
			changedContext.ControlEnvironment = nil
		case "os":
			changedContext.OS = "unsupported"
		case "intent":
			changedIntent.InputsSHA256 = strings.Repeat("0", 64)
		case "upgrade":
			changedIntent.Inputs.Kind = StartupKind("owner-upgrade")
		}
		if operation, err := newRootOwnerOperation(changedIntent, changedContext); !errors.Is(err, ErrIdentity) || operation != (Operation{}) {
			t.Fatalf("invalid %s operation accepted: operation=%+v error=%v", kind, operation, err)
		}
	}
}
