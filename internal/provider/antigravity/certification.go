package antigravity

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// certificationJSON is permission evidence for a specific executable/profile,
// not a claim that later interpreter changes have completed release testing.
//
//go:embed certification.json
var certificationJSON []byte

type certificationRecord struct {
	SchemaVersion   int    `json:"schema_version"`
	Status          string `json:"status"`
	Provider        string `json:"provider"`
	ProviderVersion string `json:"provider_version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	RuntimeSHA256   string `json:"runtime_sha256"`
	ProfileRevision string `json:"profile_revision"`
	Mode            string `json:"mode"`
	Approval        string `json:"approval"`
	NativeEvidence  struct {
		Version string `json:"predicate_version"`
		SHA256  string `json:"predicate_sha256"`
	} `json:"native_evidence"`
}

// PrepareCertified refuses an unexecuted runtime/profile before admission.
// It resolves local policy without starting a provider or requiring local
// certification artifacts. Collection does not call this launch gate.
func PrepareCertified(request task.TaskRecord) (Prepared, error) {
	prepared, err := PrepareCandidate(request)
	if err != nil {
		return Prepared{}, err
	}
	if err = validateCertification(prepared, runtime.GOOS, runtime.GOARCH); err != nil {
		return Prepared{}, err
	}
	return prepared, nil
}

func validateCertification(prepared Prepared, osName, arch string) error {
	record, err := embeddedCertification()
	if err != nil {
		return err
	}
	if !prepared.Plan.Predicate.Equal(record.predicate()) {
		return fmt.Errorf("%w: predicate lacks Executed certification", ErrUnsupportedProfile)
	}
	policy := prepared.Effective.Policy
	if record.SchemaVersion != 1 || record.Status != "Executed" || record.Provider != Provider ||
		record.ProviderVersion != prepared.ObservedVersion || record.OS != osName || record.Arch != arch ||
		record.Mode != prepared.Effective.Containment || record.Approval != prepared.Effective.Approval ||
		policy == nil || record.ProfileRevision != policy.ProfileRevision || record.RuntimeSHA256 != policy.RuntimeSHA256 {
		return fmt.Errorf("%w: runtime or effective profile lacks Executed certification", ErrUnsupportedProfile)
	}
	return nil
}

func embeddedCertification() (certificationRecord, error) {
	var record certificationRecord
	if err := json.Unmarshal(certificationJSON, &record); err != nil {
		return certificationRecord{}, fmt.Errorf("%w: invalid embedded certification", ErrUnsupportedProfile)
	}
	return record, nil
}

// Description returns the immutable metadata used by the production catalog.
// It is derived from the same embedded certification record used by the launch
// gate, so discovery cannot drift from the certified version/profile facts.
func Description() commonprovider.Description {
	record, err := embeddedCertification()
	if err != nil {
		panic(err) // checked-in certification is a build invariant
	}
	return commonprovider.Description{
		ID:               record.Provider,
		SupportedOptions: []string{commonprovider.OptionContinuation, commonprovider.OptionNativeTimeout},
		Discoverable:     true,
		Profiles: []commonprovider.CertifiedProfile{{
			Mode:            record.Mode,
			Approval:        record.Approval,
			Status:          record.Status,
			ProviderVersion: record.ProviderVersion,
			OS:              record.OS,
			Arch:            record.Arch,
			RuntimeSHA256:   record.RuntimeSHA256,
			ProfileRevision: record.ProfileRevision,
			Predicate:       record.predicate(),
		}},
	}
}

// Registration returns agy's one explicit catalog entry. Both predicate
// revisions remain available for historical collection; only the current
// revision is selected by fresh preparation.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:  Description(),
		Prepare:      Prepare,
		Interpreters: []predicate.Interpreter{NewPrintInterpreter(), NewCurrentPrintInterpreter()},
	}
}

func (r certificationRecord) predicate() task.PredicateRef {
	return task.PredicateRef{Adapter: r.Provider, Mode: r.Mode, Version: r.NativeEvidence.Version, SHA256: r.NativeEvidence.SHA256}
}
