package claude

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	ProfileRevision = "claude-2.1.270-darwin-arm64-read-only-1"
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
		registration.Prepare = Prepare
	}
	return registration
}

func validateCertification(prepared commonprovider.PreparedProfile, record commonprovider.CertifiedProfile, osName, arch string) error {
	if !record.MatchesPrepared(prepared, osName, arch) {
		return fmt.Errorf("%w: Claude runtime, policy or interpreter lacks Executed certification", ErrUnsupportedProfile)
	}
	return nil
}

// Prepare is the production launch gate. Candidate preparation is also used
// by the explicit acceptance composition before the first certification.
func Prepare(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	record, err := certifiedProfile()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if record.Status != "Executed" {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: Claude native qualification is pending", ErrUnsupportedProfile)
	}
	prepared, err := PrepareCandidate(request)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	finalize := prepared.Finalize
	prepared.Finalize = func(facts json.RawMessage, now time.Time) (commonprovider.PreparedProfile, error) {
		profile, finalErr := finalize(facts, now)
		if finalErr != nil {
			return commonprovider.PreparedProfile{}, finalErr
		}
		if finalErr = validateCertification(profile, record, runtime.GOOS, runtime.GOARCH); finalErr != nil {
			return commonprovider.PreparedProfile{}, finalErr
		}
		return profile, nil
	}
	return prepared, nil
}
