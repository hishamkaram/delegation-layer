package supervisor

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// infrastructureRuntimeSource owns an immutable selected runner custody
// reference, not running-image, birth-chain or daemon-start authority. Trusted
// outside composition supplies ResolveRunner("", deps.RunnerExecutable), never
// the task's custom runner selection or an RPC-supplied executable path. A
// resident supplies its verified running custody path directly, without lookup.
type infrastructureRuntimeSource struct {
	root       string
	executable string
	sha256     string
}

func prepareInfrastructureRuntimeSource(ctx context.Context, root, candidate string) (infrastructureRuntimeSource, error) {
	if err := infrastructureRuntimeContext(ctx, root); err != nil {
		return infrastructureRuntimeSource{}, err
	}
	digest, err := infrastructureRuntimeCandidate(candidate)
	if err != nil {
		return infrastructureRuntimeSource{}, err
	}
	if err = ctx.Err(); err != nil {
		return infrastructureRuntimeSource{}, err
	}
	executable, err := InstallStateRunner(root, candidate)
	if err != nil {
		return infrastructureRuntimeSource{}, err
	}
	source := infrastructureRuntimeSource{root: root, executable: executable, sha256: digest}
	if err := source.validate(ctx); err != nil {
		return infrastructureRuntimeSource{}, err
	}
	return source, nil
}

func infrastructureRuntimeContext(ctx context.Context, root string) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("%w: infrastructure runtime root must be canonical and absolute", pueue.ErrConfiguration)
	}
	return nil
}

func infrastructureRuntimeCandidate(candidate string) (string, error) {
	if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
		return "", fmt.Errorf("%w: infrastructure runtime candidate must be canonical and absolute", pueue.ErrConfiguration)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || resolved != candidate {
		return "", errors.Join(fmt.Errorf("%w: infrastructure runtime candidate must not alias another path", pueue.ErrConfiguration), err)
	}
	digest, err := stateExecutableSourceDigest(candidate)
	if err != nil {
		return "", err
	}
	info, err := buildinfo.ReadFile(candidate)
	if err != nil || !ManagedRunnerBuildInfo(info) {
		return "", errors.Join(fmt.Errorf("%w: infrastructure runtime candidate is not a managed runner", pueue.ErrConfiguration), err)
	}
	return digest, nil
}

func (s infrastructureRuntimeSource) validate(ctx context.Context) error {
	if err := infrastructureRuntimeContext(ctx, s.root); err != nil {
		return err
	}
	if task.ValidateSHA256(s.sha256) != nil || s.executable != filepath.Join(s.root, ".supervisor", stateRunnerName+"-"+s.sha256) {
		return fmt.Errorf("%w: invalid infrastructure runtime custody identity", pueue.ErrConfiguration)
	}
	if err := validateStateSupervisorDirectory(filepath.Join(s.root, ".supervisor")); err != nil {
		return err
	}
	digest, err := protectedStateExecutableDigest(s.executable)
	if err != nil {
		return err
	}
	if digest != s.sha256 || !CurrentManagedRunnerExecutable(s.root, s.executable) {
		return fmt.Errorf("%w: infrastructure runtime custody changed", pueue.ErrConfiguration)
	}
	eligibleDigest, err := infrastructureRuntimeCandidate(s.executable)
	if err != nil {
		return err
	}
	if eligibleDigest != s.sha256 {
		return pueue.ErrConfiguration
	}
	return ctx.Err()
}

// stage derives a target custody source from these frozen bytes. It performs no
// ambient lookup and makes no claim about which process is executing the image.
// Composition must retain a validated, pinned target through the copy; its path
// alone supplies neither root identity nor mutation authority.
func (s infrastructureRuntimeSource) stage(ctx context.Context, targetRoot string) (infrastructureRuntimeSource, error) {
	if err := s.validate(ctx); err != nil {
		return infrastructureRuntimeSource{}, err
	}
	if err := infrastructureRuntimeContext(ctx, targetRoot); err != nil {
		return infrastructureRuntimeSource{}, err
	}
	executable, err := InstallStateRunner(targetRoot, s.executable)
	if err != nil {
		return infrastructureRuntimeSource{}, err
	}
	staged := infrastructureRuntimeSource{root: targetRoot, executable: executable, sha256: s.sha256}
	if err := staged.validate(ctx); err != nil {
		return infrastructureRuntimeSource{}, err
	}
	return staged, nil
}
