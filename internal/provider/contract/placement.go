package contract

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

// ErrProfileUnavailable identifies a provider mode or runtime that cannot
// be prepared for this request.
var ErrProfileUnavailable = errors.New("provider permission profile is not available")

// PathObservation is a supplied filesystem fact about one absolute path: the
// canonical spelling an observer resolved for it, or why resolution failed. It
// is a value, not a cache; the observer that produced it owns freshness.
type PathObservation struct {
	Path      string
	Canonical string
	Err       error
}

func (o PathObservation) isCanonical() bool {
	return o.Err == nil && o.Canonical == o.Path && filepath.IsAbs(o.Path) &&
		filepath.Clean(o.Path) == o.Path && !strings.ContainsRune(o.Path, 0)
}

// StateRoot is a Delegate state root already accepted as canonical. It is the
// only way to check provider-writable roots, so the root is always judged
// before any writable root.
type StateRoot struct {
	path string
}

// NewStateRoot accepts a state root only when its observation shows the
// supplied spelling is already canonical.
func NewStateRoot(root PathObservation) (StateRoot, error) {
	if !root.isCanonical() {
		return StateRoot{}, fmt.Errorf("%w: state root is not canonical", ErrProfileUnavailable)
	}
	return StateRoot{path: root.Path}, nil
}

// CheckWritable rejects a provider-writable root that is not canonical or that
// overlaps the state root in either direction.
func (r StateRoot) CheckWritable(writable PathObservation) error {
	if r.path == "" {
		return fmt.Errorf("%w: state root is not canonical", ErrProfileUnavailable)
	}
	if !writable.isCanonical() {
		return fmt.Errorf("%w: provider writable root is not canonical: %s", ErrProfileUnavailable, writable.Path)
	}
	if policy.PathsOverlap(r.path, writable.Path) {
		return fmt.Errorf("%w: state root overlaps provider-writable tree %s", ErrProfileUnavailable, writable.Path)
	}
	return nil
}
