package task

import (
	"fmt"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/filepaths"
)

// RemoteBinding pins the originating intent; it grants no launch authority.
type RemoteBinding struct {
	OriginRootID string   `json:"origin_root_id"`
	IntentSHA256 string   `json:"intent_sha256"`
	InputSHA256  string   `json:"input_sha256,omitempty"`
	OutputPaths  []string `json:"output_paths,omitempty"`
}

// Clone returns an independently owned immutable binding value.
func (b *RemoteBinding) Clone() *RemoteBinding {
	if b == nil {
		return nil
	}
	result := *b
	result.OutputPaths = slices.Clone(b.OutputPaths)
	return &result
}

func validateRemoteBinding(b *RemoteBinding) error {
	if b == nil {
		return nil
	}
	if err := ValidateRootID(b.OriginRootID); err != nil {
		return err
	}
	if err := ValidateSHA256(b.IntentSHA256); err != nil {
		return err
	}
	if b.InputSHA256 != "" {
		if err := ValidateSHA256(b.InputSHA256); err != nil {
			return err
		}
	}
	if err := filepaths.ValidateSelectors(b.OutputPaths); err != nil {
		return err
	}
	if !slices.IsSorted(b.OutputPaths) {
		return fmt.Errorf("remote output paths must be ordered")
	}
	return nil
}
