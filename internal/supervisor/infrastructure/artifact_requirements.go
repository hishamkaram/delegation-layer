package infrastructure

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrIncompatible = errors.New("infrastructure artifacts incompatible")

// ArtifactRequirements is the request's immutable compatibility constraint.
// The resident owns source selection and verification; these hashes confer no
// executable provenance or permission to select paths or launch processes.
type ArtifactRequirements struct {
	RequiredRuntimeSHA256 string `json:"required_runtime_sha256"`
	RequiredClientSHA256  string `json:"required_client_sha256"`
	RequiredDaemonSHA256  string `json:"required_daemon_sha256"`
}

func (r ArtifactRequirements) Validate() error {
	for _, digest := range []string{r.RequiredRuntimeSHA256, r.RequiredClientSHA256, r.RequiredDaemonSHA256} {
		if err := task.ValidateSHA256(digest); err != nil {
			return errors.Join(ErrIdentity, err)
		}
	}
	return nil
}

// ValidateArtifacts compares hashes of sources already verified by the caller.
// Matching constraints alone do not validate paths, bytes or native authority.
func (r ArtifactRequirements) ValidateArtifacts(actual Artifacts) error {
	if err := r.Validate(); err != nil {
		return err
	}
	for _, member := range []struct{ name, required, actual string }{
		{"runtime", r.RequiredRuntimeSHA256, actual.RunnerSHA256},
		{"client", r.RequiredClientSHA256, actual.ClientSHA256},
		{"daemon", r.RequiredDaemonSHA256, actual.DaemonSHA256},
	} {
		if err := task.ValidateSHA256(member.actual); err != nil {
			return errors.Join(ErrIdentity, err)
		}
		if member.required != member.actual {
			return fmt.Errorf("%w: required %s SHA-256 differs from verified source", ErrIncompatible, member.name)
		}
	}
	return nil
}
