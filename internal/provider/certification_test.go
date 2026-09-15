package provider

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestCertificationMatchesAllLaunchAndInterpretationFacts(t *testing.T) {
	// Injected fixture certification is never written to the native receipt.
	record := CertifiedProfile{
		Status: "Executed", Mode: "read-only", Approval: "never", ProviderVersion: "test-version",
		OS: "darwin", Arch: "arm64", RuntimeSHA256: task.ComputeSHA256([]byte("runtime")),
		ProfileRevision: "test-profile", Predicate: task.FixturePredicateRef(), OutputWriterContract: task.OutputWriterProcessExitEOF,
	}
	prepared := PreparedProfile{
		ObservedVersion: "test-version",
		Plan:            execution.Plan{Predicate: task.FixturePredicateRef(), OutputWriterContract: task.OutputWriterProcessExitEOF},
		Effective: task.EffectiveConfig{Containment: "read-only", Approval: "never", Policy: &task.PolicyDetails{
			RuntimeSHA256: task.ComputeSHA256([]byte("runtime")), ProfileRevision: "test-profile",
		}},
	}
	if !record.MatchesPrepared(prepared, "darwin", "arm64") {
		t.Fatal("matching certification rejected")
	}
	cases := map[string]func(*CertifiedProfile){
		"unexecuted":   func(r *CertifiedProfile) { r.Status = "Pending" },
		"version":      func(r *CertifiedProfile) { r.ProviderVersion = "0.155.0" },
		"platform":     func(r *CertifiedProfile) { r.OS = "linux" },
		"architecture": func(r *CertifiedProfile) { r.Arch = "amd64" },
		"permission":   func(r *CertifiedProfile) { r.Mode = "workspace-write" },
		"approval":     func(r *CertifiedProfile) { r.Approval = "on-request" },
		"executable": func(r *CertifiedProfile) {
			r.RuntimeSHA256 = task.ComputeSHA256([]byte("different executable"))
		},
		"profile": func(r *CertifiedProfile) { r.ProfileRevision += "-changed" },
		"predicate": func(r *CertifiedProfile) {
			r.Predicate.SHA256 = task.ComputeSHA256([]byte("different interpretation"))
		},
		"writer": func(r *CertifiedProfile) { r.OutputWriterContract = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := record
			mutate(&changed)
			if changed.MatchesPrepared(prepared, "darwin", "arm64") {
				t.Fatal("mismatched certification accepted")
			}
		})
	}
	prepared.Effective.Policy = nil
	if record.MatchesPrepared(prepared, "darwin", "arm64") {
		t.Fatal("missing policy accepted")
	}
}
