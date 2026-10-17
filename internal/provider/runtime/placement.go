// Package runtime is the provider filesystem edge. It observes paths and hands
// the facts to the pure provider contract; it owns no placement policy.
package runtime

import (
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/provider/contract"
)

// ObservePath resolves the canonical spelling of an absolute path.
func ObservePath(path string) contract.PathObservation {
	canonical, err := config.CanonicalizePath(path)
	return contract.PathObservation{Path: path, Canonical: canonical, Err: err}
}

// ValidateStatePlacement rejects a state root that is not canonical or that
// overlaps any provider-writable tree. The root is observed and judged before
// any provider root, and each provider root is observed and judged before the
// next, so the first failure in order is the one reported.
func ValidateStatePlacement(stateRoot string, providerRoots []string) error {
	root, err := contract.NewStateRoot(ObservePath(stateRoot))
	if err != nil {
		return err
	}
	for _, writable := range providerRoots {
		if err = root.CheckWritable(ObservePath(writable)); err != nil {
			return err
		}
	}
	return nil
}
