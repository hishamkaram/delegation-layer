package control

import (
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// IsLegacyStateSupervisorClient recognizes a validated historical reference's
// reserved layout, without opening the copied executable or granting custody.
func IsLegacyStateSupervisorClient(root string, saved task.SupervisorRef) bool {
	if saved.ExecutableIdentity != "" || !filepath.IsAbs(root) || filepath.Clean(root) != root ||
		saved.ConfigPath != PrivateConfigPath(root) || task.ValidateFreshSupervisorRef(saved) != nil {
		return false
	}
	return isLegacySupervisorClientPath(root, saved.ClientExecutable)
}

func isLegacySupervisorClientPath(root, path string) bool {
	if filepath.Clean(path) != path || filepath.Base(path) != "pueue" {
		return false
	}
	directory := filepath.Dir(path)
	if filepath.Dir(directory) != filepath.Join(root, privateSupervisorDirectory) {
		return false
	}
	const prefix = "pueue-pair-"
	name := filepath.Base(directory)
	return strings.HasPrefix(name, prefix) && task.ValidateSHA256(strings.TrimPrefix(name, prefix)) == nil
}

func isLegacyStateSupervisorClient(saved task.SupervisorRef) bool {
	root := filepath.Dir(filepath.Dir(saved.ConfigPath))
	return IsLegacyStateSupervisorClient(root, saved)
}

// IsLegacyStateRunnerPath recognizes only the reserved historical runner-copy
// layout. This is refusal/classification data, never execution ownership.
func IsLegacyStateRunnerPath(root, path string) bool {
	if filepath.Dir(path) != filepath.Join(root, privateSupervisorDirectory) {
		return false
	}
	name := filepath.Base(path)
	if name == "delegate-run" {
		return true
	}
	const prefix = "delegate-run-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	digest := strings.TrimPrefix(name, prefix)
	if len(digest) != 64 {
		return false
	}
	for _, value := range digest {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}
