package codex

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestCertificationBindsAllLaunchAndInterpretationFacts(t *testing.T) {
	// Injected fixture certification is never written to the native receipt.
	record := commonprovider.CertifiedProfile{
		Status: "Executed", Mode: Mode, Approval: "never", ProviderVersion: Version,
		OS: "darwin", Arch: "arm64", RuntimeSHA256: inspectedRuntimeSHA256,
		ProfileRevision: ProfileRevision, Predicate: Reference(), OutputWriterContract: OutputWriterContract,
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
	cases := map[string]func(*commonprovider.CertifiedProfile){
		"unexecuted":   func(r *commonprovider.CertifiedProfile) { r.Status = "Pending" },
		"version":      func(r *commonprovider.CertifiedProfile) { r.ProviderVersion = "0.155.0" },
		"platform":     func(r *commonprovider.CertifiedProfile) { r.OS = "linux" },
		"architecture": func(r *commonprovider.CertifiedProfile) { r.Arch = "amd64" },
		"permission":   func(r *commonprovider.CertifiedProfile) { r.Mode = "workspace-write" },
		"approval":     func(r *commonprovider.CertifiedProfile) { r.Approval = "on-request" },
		"executable": func(r *commonprovider.CertifiedProfile) {
			r.RuntimeSHA256 = task.ComputeSHA256([]byte("different executable"))
		},
		"profile": func(r *commonprovider.CertifiedProfile) { r.ProfileRevision += "-changed" },
		"predicate": func(r *commonprovider.CertifiedProfile) {
			r.Predicate.SHA256 = task.ComputeSHA256([]byte("different interpretation"))
		},
		"writer": func(r *commonprovider.CertifiedProfile) { r.OutputWriterContract = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := record
			mutate(&changed)
			if err := validateCertification(prepared, changed, "darwin", "arm64"); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("got %v", err)
			}
		})
	}
	prepared.Effective.Policy = nil
	if err := validateCertification(prepared, record, "darwin", "arm64"); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("missing policy accepted: %v", err)
	}
}

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
