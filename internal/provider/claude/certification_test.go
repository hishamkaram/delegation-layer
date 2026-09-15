package claude

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestCertificationKeepsUnqualifiedClaudeHistoricalOnly(t *testing.T) {
	record, err := certifiedProfile()
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "Pending" {
		t.Fatalf("qualification expectation must follow the native receipt: %s", record.Status)
	}
	if !record.Predicate.Equal(Reference()) || record.ProfileRevision != ProfileRevision || record.ProviderVersion != Version || record.RuntimeSHA256 != inspectedRuntimeSHA256 {
		t.Fatal("embedded candidate metadata drifted")
	}
	registration := Registration()
	if registration.Description.Discoverable || registration.Prepare != nil || len(registration.Description.Profiles) != 0 || len(registration.Interpreters) != 1 {
		t.Fatal("unqualified provider exposes a production launch")
	}
	if _, err = Prepare(task.TaskRecord{}); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("pending production prepare=%v", err)
	}
	// The common matcher owns exhaustive field-binding tests. This provider
	// test checks only its wrapper and its immutable embedded facts.
	prepared := commonprovider.PreparedProfile{ObservedVersion: Version, Plan: execution.Plan{Predicate: Reference()}, Effective: task.EffectiveConfig{Containment: Mode, Approval: "dontAsk", Policy: &task.PolicyDetails{RuntimeSHA256: inspectedRuntimeSHA256, ProfileRevision: ProfileRevision}}}
	if err = validateCertification(prepared, record, "darwin", "arm64"); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("pending certification accepted: %v", err)
	}
	record.Status = "Executed"
	if err = validateCertification(prepared, record, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
}
