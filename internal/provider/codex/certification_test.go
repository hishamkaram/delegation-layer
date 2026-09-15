package codex

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestEmbeddedCertificationMatchesShippedInterpreterAndProfile(t *testing.T) {
	record, err := certifiedProfile()
	if err != nil {
		t.Fatal(err)
	}
	prepared := commonprovider.PreparedProfile{
		ObservedVersion: Version,
		Plan:            execution.Plan{Predicate: Reference(), OutputWriterContract: OutputWriterContract},
		Effective: task.EffectiveConfig{Containment: Mode, Approval: "never", Policy: &task.PolicyDetails{
			RuntimeSHA256: inspectedRuntimeSHA256, ProfileRevision: ProfileRevision,
		}},
	}
	if err := validateCertification(prepared, record, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	registration := Registration()
	if !registration.Description.Discoverable || registration.Prepare == nil {
		t.Fatal("qualified provider lacks production discovery or preparation")
	}
}
