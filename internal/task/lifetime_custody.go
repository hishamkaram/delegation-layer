package task

import (
	"errors"
	"fmt"
)

// LifetimeCustodyPlacementVersion is the only custody placement wire version.
const LifetimeCustodyPlacementVersion = 1

// LifetimeCustodyPlacement names the custody pool that owns a registered
// scope. A registration without it predates placement; this type alone never
// authorizes a fallback or cleanup for such a registration.
//
// PoolRoot is a lexically clean absolute path. PoolID is an opaque random
// 64-lowercase-hex nonce, not a content hash. Physical canonicalization and
// filesystem identity are I/O checks the owning store makes against the real
// pool; validation here never touches the filesystem.
type LifetimeCustodyPlacement struct {
	Version  int    `json:"version"`
	PoolRoot string `json:"pool_root"`
	PoolID   string `json:"pool_id"`
}

// ValidateLifetimeCustodyPlacement checks the placement's shape only.
func ValidateLifetimeCustodyPlacement(p *LifetimeCustodyPlacement) error {
	if p == nil {
		return errors.New("nil lifetime custody placement")
	}
	if p.Version != LifetimeCustodyPlacementVersion {
		return fmt.Errorf("%w: lifetime custody placement version %d", ErrUnsupportedSchema, p.Version)
	}
	if !IsCanonicalCwd(p.PoolRoot) {
		return fmt.Errorf("%w: lifetime custody pool root must be a nonempty, absolute, clean path", ErrIdentityMismatch)
	}
	if err := ValidateSHA256(p.PoolID); err != nil {
		return fmt.Errorf("lifetime custody pool id: %w", err)
	}
	return nil
}

// Clone returns an unaliased snapshot of an optional placement, preserving
// nil for a registration that predates placement. It copies the value the
// caller holds; it does not make concurrent mutation during the copy safe.
func (placement *LifetimeCustodyPlacement) Clone() *LifetimeCustodyPlacement {
	if placement == nil {
		return nil
	}
	snapshot := *placement
	return &snapshot
}
