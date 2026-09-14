package antigravity

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDescriptionDerivesCertifiedMetadataFromEmbeddedRecord(t *testing.T) {
	record, err := embeddedCertification()
	if err != nil {
		t.Fatal(err)
	}
	description := Description()
	if description.ID != record.Provider || len(description.Profiles) != 1 {
		t.Fatalf("description identity/profile count mismatch: %+v", description)
	}
	profile := description.Profiles[0]
	if profile.Mode != record.Mode || profile.Approval != record.Approval || profile.Status != record.Status ||
		profile.ProviderVersion != record.ProviderVersion || profile.OS != record.OS || profile.Arch != record.Arch ||
		profile.RuntimeSHA256 != record.RuntimeSHA256 || profile.ProfileRevision != record.ProfileRevision ||
		!profile.Predicate.Equal(record.predicate()) {
		t.Fatalf("description drifted from embedded certification: %+v, record=%+v", profile, record)
	}
	if !description.Discoverable || len(description.SupportedOptions) != 2 {
		t.Fatalf("description omitted supported production capabilities: %+v", description)
	}
}

func TestCertificationRejectsUnexecutedRuntimeAndProfile(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Prepared, *string, *string)
	}{
		{"legacy predicate", func(p *Prepared, _, _ *string) { p.Plan.Predicate = NewPrintInterpreter().Reference() }},
		{"changed predicate", func(p *Prepared, _, _ *string) {
			p.Plan.Predicate.SHA256 = task.ComputeSHA256([]byte("changed interpreter"))
		}},
		{"OS", func(_ *Prepared, osName, _ *string) { *osName = "linux" }},
		{"architecture", func(_ *Prepared, _, arch *string) { *arch = "amd64" }},
		{"version", func(p *Prepared, _, _ *string) { p.ObservedVersion = "1.2.3" }},
		{"binary", func(p *Prepared, _, _ *string) {
			p.Effective.Policy.RuntimeSHA256 = task.ComputeSHA256([]byte("other binary"))
		}},
		{"profile", func(p *Prepared, _, _ *string) { p.Effective.Policy.ProfileRevision = "unexecuted" }},
		{"approval", func(p *Prepared, _, _ *string) { p.Effective.Approval = "auto-approve" }},
		{"mode", func(p *Prepared, _, _ *string) { p.Effective.Containment = "read-only" }},
		{"missing policy", func(p *Prepared, _, _ *string) { p.Effective.Policy = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Prepared{Plan: execution.Plan{Predicate: NewCurrentPrintInterpreter().Reference()}, ObservedVersion: Version, Effective: task.EffectiveConfig{Containment: Mode, Approval: "accept-edits:request-review:headless-deny", Policy: &task.PolicyDetails{ProfileRevision: ProfileRevision, RuntimeSHA256: inspectedDarwinARM64SHA256}}}
			osName, arch := "darwin", "arm64"
			if err := validateCertification(p, osName, arch); err != nil {
				t.Fatalf("executed baseline rejected: %v", err)
			}
			tc.change(&p, &osName, &arch)
			if err := validateCertification(p, osName, arch); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("unexecuted case admitted: %v", err)
			}
		})
	}
}
