//go:build !darwin

package codex

import "github.com/hishamkaram/delegation-layer/internal/task"

func managedSources() ([]task.PolicySourceDigest, error) {
	// Darwin exposes an additional managed-preference API. Portable policy
	// files are still inventoried by policy.go; unavailable native metadata is
	// optional and must not make the provider unsupported.
	return nil, nil
}
