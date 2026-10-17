package contract

import (
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ValidateArtifact owns the common descriptor shape. Capture additionally
// requires complete bytes; verification may retain an explicitly marked prefix.
func ValidateArtifact(artifact Artifact) error {
	if err := ValidateArtifactName(artifact.Name); err != nil {
		return err
	}
	if artifact.Length < 0 {
		return task.ErrIdentityMismatch
	}
	if artifact.SHA256 != "" {
		if err := task.ValidateSHA256(artifact.SHA256); err != nil {
			return err
		}
	}
	return validateArtifactRetention(artifact)
}

func ValidateArtifactName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 64 || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateArtifactRetention(artifact Artifact) error {
	if artifact.Omitted != "" {
		if artifact.Omitted != "limit" || artifact.Length != 0 || artifact.SHA256 != "" || artifact.Truncated {
			return task.ErrIdentityMismatch
		}
		return nil
	}
	if artifact.Length > ArtifactLimit || artifact.SHA256 == "" {
		return task.ErrIdentityMismatch
	}
	return nil
}
