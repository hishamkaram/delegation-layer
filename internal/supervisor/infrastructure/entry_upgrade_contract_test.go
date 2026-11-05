package infrastructure

import (
	"errors"
	"strings"
	"testing"
)

// Immutable data fixture only: neither the body nor a constructed Operation
// proves Exiting, an instance lease, native placement or permission to Start.
func upgradeOperationFixture(t *testing.T) (StartupIntent, LaunchContext, epochLifecycleIntent) {
	t.Helper()
	owner, selected := rootOperationFixture(t)
	ownerRef, err := ReferenceStartup(owner)
	if err != nil {
		t.Fatal(err)
	}
	body := epochLifecycleIntent{
		Protocol: ProtocolVersion, Sequence: 1, ID: strings.Repeat("e", 32), Kind: epochLifecycleOwnerUpgrade,
		Root: owner.Inputs.Root, RootID: owner.Inputs.RootID, BootID: owner.Inputs.BootID,
		RootDevice: owner.Inputs.RootDevice, RootInode: owner.Inputs.RootInode, Owner: ownerRef,
		AcceptedAt: owner.AcceptedAt, Deadline: owner.Deadline,
		SuccessorArtifacts: owner.Inputs.Artifacts, SuccessorContext: owner.Inputs.Context,
	}
	reference, err := referenceEpochLifecycle(body)
	if err != nil {
		t.Fatal(err)
	}
	successor := owner
	successor.Sequence++
	successor.Epoch = strings.Repeat("f", 32)
	successor.Inputs.Kind = StartupUpgrade
	successor.Inputs.Artifacts, successor.Inputs.Context = Artifacts{}, LaunchContextReference{}
	successor.Inputs.UpgradeID, successor.Inputs.UpgradeSHA256 = reference.ID, reference.SHA256
	refreshIntentDigest(t, &successor)
	return successor, selected, body
}

func TestUpgradeOperationDerivesOnlyReferencedLifecycleSelection(t *testing.T) {
	intent, selected, body := upgradeOperationFixture(t)
	operation, err := newUpgradeOperation(intent, selected, body)
	if err != nil {
		t.Fatal(err)
	}
	selected.ControlEnvironment[0] = "changed=value"
	originalArtifacts, originalContext := body.SuccessorArtifacts, body.SuccessorContext
	body.SuccessorArtifacts.RunnerSHA256 = strings.Repeat("b", 64)
	if operation.Intent() != intent || operation.Artifacts() != originalArtifacts || operation.defaultContextReference() != originalContext {
		t.Fatal("upgrade introduced another artifact/context selection")
	}
	if err = operation.Validate(); err != nil {
		t.Fatalf("constructor input changed immutable operation: %v", err)
	}
	if intent.Inputs.Artifacts != (Artifacts{}) || intent.Inputs.Context != (LaunchContextReference{}) {
		t.Fatal("startup duplicated lifecycle-owned selection")
	}
}

func TestUpgradeOperationRejectsChangedBodyAndIndependentSelection(t *testing.T) {
	rootIntent, rootContext := rootOperationFixture(t)
	if _, err := newUpgradeOperation(rootIntent, rootContext, epochLifecycleIntent{}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("upgrade constructor accepted unrelated root-owner operation: %v", err)
	}
	for _, changed := range []string{"body", "body-reference", "root", "context", "os", "independent-artifacts", "root-owner-body"} {
		t.Run(changed, func(t *testing.T) {
			intent, selected, body := upgradeOperationFixture(t)
			switch changed {
			case "body":
				body.SuccessorArtifacts.RunnerSHA256 = strings.Repeat("b", 64)
			case "body-reference":
				intent.Inputs.UpgradeSHA256 = strings.Repeat("b", 64)
				refreshIntentDigest(t, &intent)
			case "root":
				selected.Root += "-foreign"
			case "context":
				selected.ControlEnvironment[0] = "foreign=value"
			case "os":
				selected.OS = "unsupported"
			case "independent-artifacts":
				intent.Inputs.Artifacts = body.SuccessorArtifacts
			case "root-owner-body":
				intent.Inputs.Kind = StartupRootOwner
				intent.Inputs.Artifacts, intent.Inputs.Context = body.SuccessorArtifacts, body.SuccessorContext
				intent.Inputs.UpgradeID, intent.Inputs.UpgradeSHA256 = "", ""
				refreshIntentDigest(t, &intent)
			}
			if _, err := newUpgradeOperation(intent, selected, body); !errors.Is(err, ErrIdentity) {
				t.Fatalf("accepted changed %s: %v", changed, err)
			}
		})
	}
}
