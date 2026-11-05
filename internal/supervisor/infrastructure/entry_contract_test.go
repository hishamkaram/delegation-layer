package infrastructure

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func rootOperationFixture(t *testing.T) (StartupIntent, LaunchContext) {
	t.Helper()
	intent := startupIntentFixture(t)
	launchContext := LaunchContext{Protocol: ProtocolVersion, Root: intent.Inputs.Root, RootID: intent.Inputs.RootID, OS: runtime.GOOS, ControlEnvironment: []string{"PROVIDER=selected"}, MaintenanceEnvironment: []string{}}
	digest, err := launchContext.Digest()
	if err != nil {
		t.Fatal(err)
	}
	intent.Inputs.Context = LaunchContextReference{SHA256: digest}
	refreshIntentDigest(t, &intent)
	return intent, launchContext
}

func TestRootOwnerOperationOwnsContextAndDerivesArtifacts(t *testing.T) {
	intent, launchContext := rootOperationFixture(t)
	operation, err := newRootOwnerOperation(intent, launchContext)
	if err != nil {
		t.Fatal(err)
	}
	launchContext.ControlEnvironment[0] = "changed"
	if operation.context.ControlEnvironment[0] != "PROVIDER=selected" || operation.Intent() != intent || operation.Artifacts() != intent.Inputs.Artifacts {
		t.Fatal("operation changed source records or aliases constructor input")
	}
	if err = operation.Validate(); err != nil {
		t.Fatalf("constructor input changed immutable birth context: %v", err)
	}
}

func TestRootOwnerOperationRefusesZeroForeignAndUpgrade(t *testing.T) {
	if err := (Operation{}).Validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("zero operation accepted: %v", err)
	}
	intent, launchContext := rootOperationFixture(t)
	for _, kind := range []string{"root", "root-id", "context", "os", "upgrade"} {
		changedIntent, changedContext := intent, launchContext.Clone()
		switch kind {
		case "root":
			changedContext.Root = "/different"
		case "root-id":
			changedContext.RootID = strings.Repeat("e", 32)
		case "context":
			changedContext.ControlEnvironment[0] = "different=value"
		case "os":
			changedContext.OS = "unsupported"
		case "upgrade":
			changedIntent.Inputs.Kind = StartupUpgrade
			changedIntent.Inputs.Artifacts = Artifacts{}
			changedIntent.Inputs.Context = LaunchContextReference{}
			changedIntent.Inputs.UpgradeID = strings.Repeat("a", 32)
			changedIntent.Inputs.UpgradeSHA256 = strings.Repeat("b", 64)
			refreshIntentDigest(t, &changedIntent)
		}
		if _, err := newRootOwnerOperation(changedIntent, changedContext); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid %s operation accepted: %v", kind, err)
		}
	}
}
