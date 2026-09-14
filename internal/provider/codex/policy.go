package codex

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func effectivePolicy(request task.TaskRecord, environment profileEnvironment, now time.Time, managed func() ([]task.PolicySourceDigest, error)) (task.EffectiveConfig, error) {
	sources, err := managed()
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	files, err := policyFiles(request.CanonicalCwd)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	local, err := emptyConfigSources(files)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	sources = append(sources, local...)
	auth, err := personalAuthSource(environment.CodexHome, now, time.Duration(request.BudgetNanos))
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	sources = append(sources, auth)
	return sealPolicy(request, environment, sources)
}

func emptyConfigSources(paths []string) ([]task.PolicySourceDigest, error) {
	var sources []task.PolicySourceDigest
	for _, path := range paths {
		source, readErr := commonprovider.ReadPolicySource(path)
		if readErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedProfile, readErr)
		}
		if len(source.Data) != 0 {
			return nil, fmt.Errorf("%w: nonempty system/project configuration: %s", ErrUnsupportedProfile, path)
		}
		receipt := task.PolicySourceDigest{Path: path, Kind: "empty-native-config", Present: source.Present}
		if source.Present {
			receipt.SHA256 = task.ComputeSHA256(source.Data)
		}
		sources = append(sources, receipt)
	}
	return sources, nil
}

func sealPolicy(request task.TaskRecord, environment profileEnvironment, sources []task.PolicySourceDigest) (task.EffectiveConfig, error) {
	sources = slices.Clone(sources)
	slices.SortFunc(sources, func(a, b task.PolicySourceDigest) int {
		if order := cmp.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	effective := task.EffectiveConfig{Containment: Mode, Approval: "never", Policy: &task.PolicyDetails{
		ProfileRevision: ProfileRevision, RuntimeSHA256: inspectedRuntimeSHA256,
		Workspace: request.CanonicalCwd, WritableRoots: slices.Clone(environment.WritableRoots), Sources: sources,
	}}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	effective.Digest = task.ComputeSHA256(encoded)
	if err = task.ValidateEffectiveConfig(effective); err != nil {
		return task.EffectiveConfig{}, err
	}
	return effective, nil
}

// Native default discovery visits CWD through the nearest .git marker. A
// directory marker requires HEAD. System/managed settings cannot override the
// default markers in this profile; user configuration is disabled in argv.
func policyFiles(workspace string) ([]string, error) {
	root, err := projectRoot(workspace)
	if err != nil {
		return nil, err
	}
	system, err := config.CanonicalizePath("/etc/codex")
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(system, "config.toml"), filepath.Join(system, "requirements.toml"), filepath.Join(system, "managed_config.toml")}
	for directory := workspace; ; directory = filepath.Dir(directory) {
		paths = append(paths, filepath.Join(directory, ".codex", "config.toml"))
		if len(paths) >= task.MaxPolicySources-3 {
			return nil, fmt.Errorf("%w: policy inventory exceeds the source bound", ErrUnsupportedProfile)
		}
		if directory == root {
			break
		}
	}
	return paths, nil
}

func projectRoot(workspace string) (string, error) {
	for directory := workspace; ; directory = filepath.Dir(directory) {
		valid, err := gitRootMarker(filepath.Join(directory, ".git"))
		if err != nil {
			return "", err
		}
		if valid {
			return directory, nil
		}
		if filepath.Dir(directory) == directory {
			break
		}
	}
	return "", fmt.Errorf("%w: a Git workspace is required", ErrUnsupportedProfile)
}

func gitRootMarker(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect Git marker: %w", ErrUnsupportedProfile, err)
	}
	if info.Mode().IsRegular() {
		return true, nil
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%w: unsupported Git marker type", ErrUnsupportedProfile)
	}
	head, err := os.Lstat(filepath.Join(path, "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect Git HEAD: %w", ErrUnsupportedProfile, err)
	}
	if !head.Mode().IsRegular() {
		return false, fmt.Errorf("%w: unsupported Git HEAD type", ErrUnsupportedProfile)
	}
	return true, nil
}
