package codex

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	ProfileRevision      = "codex-0.154.0-darwin-arm64-read-only-2"
	OutputWriterContract = task.OutputWriterProcessExitEOF
)

//go:embed certification.json
var certificationJSON []byte

func certifiedProfile() (commonprovider.CertifiedProfile, error) {
	var record commonprovider.CertifiedProfile
	if err := json.Unmarshal(certificationJSON, &record); err != nil {
		return record, fmt.Errorf("%w: invalid embedded certification: %w", ErrUnsupportedProfile, err)
	}
	return record, nil
}

// Registration retains interpretation independently of host availability.
// Only Executed native qualification grants production launch registration
// and discovery; historical collection remains independent of launch eligibility.
func Registration() commonprovider.Registration {
	record, err := certifiedProfile()
	if err != nil {
		panic(err)
	}
	registration := commonprovider.Registration{
		Description:  commonprovider.Description{ID: Provider, SupportedOptions: []string{commonprovider.OptionContinuation}},
		Interpreters: []predicate.Interpreter{NewInterpreter()},
	}
	if record.Status == "Executed" {
		registration.Description.Discoverable = true
		registration.Description.Profiles = []commonprovider.CertifiedProfile{record}
		registration.Prepare = commonprovider.ReadyCandidate(Prepare)
	}
	return registration
}

func validateCertification(prepared commonprovider.PreparedProfile, record commonprovider.CertifiedProfile, osName, arch string) error {
	if !record.MatchesPrepared(prepared, osName, arch) {
		return fmt.Errorf("%w: Codex runtime, policy or interpreter lacks Executed certification", ErrUnsupportedProfile)
	}
	return nil
}

// Prepare is the production launch gate. Candidate preparation is also used
// by the explicit acceptance composition before the first certification.
func Prepare(request task.TaskRecord) (commonprovider.PreparedProfile, error) {
	record, err := certifiedProfile()
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if record.Status != "Executed" {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: Codex native qualification is pending", ErrUnsupportedProfile)
	}
	prepared, err := PrepareCandidate(request)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err = validateCertification(prepared, record, runtime.GOOS, runtime.GOARCH); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepared, nil
}
