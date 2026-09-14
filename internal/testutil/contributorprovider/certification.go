package contributorprovider

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// certificationJSON is the measured, checked-in evidence for the finite
// contributor fixture. Reading it is deliberately side-effect free: catalog
// discovery must not inspect the host executable, environment, or supervisor.
//
//go:embed certification.json
var certificationJSON []byte

type certificationRecord struct {
	Status          string `json:"status"`
	ProviderVersion string `json:"provider_version"`
	RuntimeSHA256   string `json:"runtime_sha256"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	ProfileRevision string `json:"profile_revision"`
	Verification    string `json:"verification"`
}

func embeddedCertification() (certificationRecord, error) {
	var record certificationRecord
	if err := json.Unmarshal(certificationJSON, &record); err != nil {
		return certificationRecord{}, fmt.Errorf("%w: invalid embedded certification: %w", ErrUnsupportedProfile, err)
	}
	if err := record.validate(); err != nil {
		return certificationRecord{}, err
	}
	return record, nil
}

func (record certificationRecord) validate() error {
	for name, value := range map[string]string{
		"status":           record.Status,
		"provider_version": record.ProviderVersion,
		"os":               record.OS,
		"arch":             record.Arch,
		"profile_revision": record.ProfileRevision,
		"verification":     record.Verification,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: certification %s is blank", ErrUnsupportedProfile, name)
		}
	}
	if record.Status != "Executed" {
		return fmt.Errorf("%w: certification status is %q, want Executed", ErrUnsupportedProfile, record.Status)
	}
	if err := task.ValidateSHA256(record.RuntimeSHA256); err != nil {
		return fmt.Errorf("%w: certification runtime SHA-256: %w", ErrUnsupportedProfile, err)
	}
	return nil
}

func (record certificationRecord) profile() commonprovider.CertifiedProfile {
	return commonprovider.CertifiedProfile{
		Mode:                 Mode,
		Approval:             "never",
		Status:               record.Status,
		ProviderVersion:      record.ProviderVersion,
		OS:                   record.OS,
		Arch:                 record.Arch,
		RuntimeSHA256:        record.RuntimeSHA256,
		ProfileRevision:      record.ProfileRevision,
		Predicate:            Reference(),
		OutputWriterContract: OutputWriterContract,
	}
}

// Description returns the immutable discovery metadata for this finite
// provider. It contains only embedded certification and contract constants.
func Description() commonprovider.Description {
	record, err := embeddedCertification()
	if err != nil {
		panic(err) // checked-in certification is a build invariant
	}
	return commonprovider.Description{
		ID:               Provider,
		SupportedOptions: []string{commonprovider.OptionContinuation},
		Profiles:         []commonprovider.CertifiedProfile{record.profile()},
		Discoverable:     true,
	}
}

// Registration returns the one explicit synthetic contributor registration.
// Preparation performs the host-specific executable/runtime checks only when
// a task is being prepared; registration itself has no live probes.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:  Description(),
		Prepare:      Prepare,
		Interpreters: []predicate.Interpreter{NewInterpreter()},
	}
}
