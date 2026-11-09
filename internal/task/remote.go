package task

import (
	"fmt"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/filepaths"
)

// RemoteDataLayout selects a recorded byte layout, not an arbitrary path.
// Zero preserves historical record bytes and the original sibling families.
type RemoteDataLayout uint8

const (
	RemoteDataLegacy    RemoteDataLayout = 0
	RemoteDataOrganized RemoteDataLayout = 2
)

func (l RemoteDataLayout) Validate() error {
	if l != RemoteDataLegacy && l != RemoteDataOrganized {
		return fmt.Errorf("unsupported remote data layout %d", l)
	}
	return nil
}

// RemoteOriginLayout is selected once by origin snapshot preparation.
type RemoteOriginLayout struct {
	StorageLayout RemoteDataLayout `json:"storage_layout"`
}

// RemoteBinding pins the originating intent; it grants no launch authority.
type RemoteBinding struct {
	StorageLayout RemoteDataLayout `json:"storage_layout,omitempty"`
	OriginRootID  string           `json:"origin_root_id"`
	IntentSHA256  string           `json:"intent_sha256"`
	InputSHA256   string           `json:"input_sha256,omitempty"`
	OutputPaths   []string         `json:"output_paths,omitempty"`
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
	if err := b.StorageLayout.Validate(); err != nil {
		return err
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
