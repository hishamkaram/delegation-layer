package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// for-each-ref omits a dangling symbolic ref, so an inventory miss is only a
// candidate for absence. Native symref-verify with no old target refuses any
// existing ref, symbolic or direct, and accepts only an absent name.

const (
	refAbsenceProvenStdout      = "start: ok\nprepare: ok\nabort: ok\n"
	refAbsenceUnsupportedStdout = "start: ok\n"
	// update-ref dies with an untranslated "unknown command: %s" body for the
	// first record it cannot parse; LC_ALL=C pins the localized prefix.
	refAbsenceUnsupportedStderrPrefix = "fatal: unknown command: symref-verify "
	gitFatalExit                      = 128
)

var (
	errRefAbsenceVerificationUnsupported = errors.New("native Git update-ref does not support symref-verify")
	errUnexpectedRefAbsenceOutput        = errors.New("native Git ref absence verification reported unexpected output")
	errLegacyRefStorageUnprovable        = errors.New("Git ref storage is not provably the files backend")
	errHiddenOwnedRef                    = errors.New("an inventory-missing owned ref name exists in Git ref storage")
)

// verifyRefsAbsent proves that every inventory-missing expected ref is absent.
// The first actual missing name runs alone, so an older Git that cannot parse
// symref-verify fails before it reads a large input; the remaining names then
// share one batch. Only that first exact unsupported refusal may fall back to
// the files-backend presence guard. Every batch prepares and aborts, so it
// takes and releases native ref locks without changing a ref.
func (g Git) verifyRefsAbsent(ctx context.Context, commonDir string, missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	err := g.verifyRefAbsenceBatch(ctx, commonDir, missing[:1])
	if errors.Is(err, errRefAbsenceVerificationUnsupported) {
		return g.verifyLegacyFilesRefsAbsent(ctx, commonDir, missing)
	}
	if err != nil {
		return err
	}
	if len(missing) == 1 {
		return nil
	}
	if err = g.verifyRefAbsenceBatch(ctx, commonDir, missing[1:]); err != nil {
		if errors.Is(err, errRefAbsenceVerificationUnsupported) {
			return errors.Join(contract.ErrRefUnreadable, errors.New("native Git ref absence verification changed support between batches"), err)
		}
		return err
	}
	return nil
}

// verifyRefAbsenceBatch returns nil only when native Git proved every name
// absent. errRefAbsenceVerificationUnsupported is returned alone, without
// ErrRefUnreadable, only for the exact unknown-command refusal.
func (g Git) verifyRefAbsenceBatch(ctx context.Context, commonDir string, refs []string) error {
	diagnosticLocale := map[string]string{"LC_ALL": "C"}
	stdout, stderr, err := g.runOutput(ctx, commonDir, []string{"update-ref", "--stdin", "-z", "--no-deref"}, diagnosticLocale, bytes.NewReader(refAbsenceBatch(refs)))
	return classifyRefAbsenceBatch(stdout, stderr, err, refs[0])
}

func refAbsenceBatch(refs []string) []byte {
	batch := make([]byte, 0, len(refs)*80+32)
	batch = append(batch, "start\x00"...)
	for _, ref := range refs {
		batch = append(batch, "symref-verify "...)
		batch = append(batch, ref...)
		batch = append(batch, 0, 0)
	}
	return append(batch, "prepare\x00abort\x00"...)
}

// classifyRefAbsenceBatch decides one verify-only batch. Success needs a clean
// exit, the exact acknowledgements, and no diagnostic. Unsupported needs one
// ordinary fatal exit as the only cause and the exact unknown-command line for
// the submitted first name; every other result is unreadable with its cause.
func classifyRefAbsenceBatch(stdout, stderr []byte, runErr error, firstRef string) error {
	if runErr == nil {
		if string(stdout) == refAbsenceProvenStdout && len(stderr) == 0 {
			return nil
		}
		return errors.Join(contract.ErrRefUnreadable, gitCommandError(errUnexpectedRefAbsenceOutput, stderr))
	}
	if gitExitedOnlyWith(runErr, gitFatalExit) &&
		string(stdout) == refAbsenceUnsupportedStdout &&
		string(stderr) == refAbsenceUnsupportedStderrPrefix+firstRef+"\n" {
		return errRefAbsenceVerificationUnsupported
	}
	return errors.Join(contract.ErrRefUnreadable, fmt.Errorf("verify missing Git refs are absent: %w", gitCommandError(runErr, stderr)))
}

// verifyLegacyFilesRefsAbsent is the fallback for a Git without symref-verify.
// It holds only when native Git configuration proves the files backend, so
// every symbolic ref is a loose file under the common directory.
func (g Git) verifyLegacyFilesRefsAbsent(ctx context.Context, commonDir string, missing []string) error {
	if variable, set := inheritedRefBackendOverride(g.environment()); set {
		return legacyStorageUnprovable(fmt.Errorf("inherited %s may select another ref backend", variable))
	}
	if err := g.requireFilesRefStorage(ctx, commonDir); err != nil {
		return err
	}
	return looseRefsAbsent(ctx, commonDir, missing)
}

// inheritedRefBackendOverride reports an inherited variable, set to any value,
// that may select another ref backend than the repository configuration.
func inheritedRefBackendOverride(environment []string) (string, bool) {
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key == "GIT_REFERENCE_BACKEND" || key == "GIT_REF_STORAGE_FORMAT" {
			return key, true
		}
	}
	return "", false
}

// requireFilesRefStorage reads only the common directory's own configuration
// file; merged configuration could mask its ref storage. An explicit file
// does not follow includes.
func (g Git) requireFilesRefStorage(ctx context.Context, commonDir string) error {
	configPath := filepath.Join(commonDir, "config")
	stdout, stderr, err := g.runOutput(ctx, commonDir, []string{"config", "--file=" + configPath, "--get-all", "extensions.refStorage"}, nil, nil)
	return classifyRefStorageConfig(stdout, stderr, err)
}

// classifyRefStorageConfig accepts a clean unset value or exactly one plain
// files value; any other value, count, diagnostic, or fault is unprovable.
func classifyRefStorageConfig(stdout, stderr []byte, runErr error) error {
	switch {
	case runErr == nil && string(stdout) == "files\n" && len(stderr) == 0:
		return nil
	case runErr != nil && gitExitedOnlyWith(runErr, 1) && len(stdout) == 0 && len(stderr) == 0:
		return nil
	case runErr == nil:
		return legacyStorageUnprovable(fmt.Errorf("extensions.refStorage is %q", removeOneTerminatingNewline(string(stdout))))
	default:
		return legacyStorageUnprovable(gitCommandError(runErr, stderr))
	}
}

func legacyStorageUnprovable(cause error) error {
	return errors.Join(
		contract.ErrRefUnreadable,
		fmt.Errorf("%w: native symref-verify is unavailable for ref absence verification", errLegacyRefStorageUnprovable),
		cause,
	)
}

// looseRefsAbsent checks the validated common directory through one confined
// root handle. A missing component proves the files-backend name absent; any
// entry at the name, or a symlink or non-directory parent, is refused. No ref
// contents are read.
func looseRefsAbsent(ctx context.Context, commonDir string, refs []string) (resultErr error) {
	root, err := os.OpenRoot(commonDir)
	if err != nil {
		return errors.Join(contract.ErrRefUnreadable, fmt.Errorf("open Git common directory: %w", err))
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, contract.ErrRefUnreadable, fmt.Errorf("close Git common directory: %w", closeErr))
		}
	}()
	for _, ref := range refs {
		if err = ctx.Err(); err != nil {
			return errors.Join(contract.ErrRefUnreadable, err)
		}
		if err = looseRefAbsent(root, ref); err != nil {
			return err
		}
	}
	return nil
}

func looseRefAbsent(root *os.Root, ref string) error {
	if err := contract.ValidateOwnedRef(ref); err != nil {
		return err
	}
	components := strings.Split(ref, "/")
	for index := range components {
		name := strings.Join(components[:index+1], "/")
		info, err := root.Lstat(filepath.FromSlash(name))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return errors.Join(contract.ErrRefUnreadable, fmt.Errorf("inspect Git ref path %q: %w", name, err))
		}
		if index == len(components)-1 {
			return fmt.Errorf("%w: %w: %q has mode %s", contract.ErrRefUnreadable, errHiddenOwnedRef, ref, info.Mode().Type())
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: Git ref path parent %q is not a plain directory", contract.ErrRefUnreadable, name)
		}
	}
	return nil
}
