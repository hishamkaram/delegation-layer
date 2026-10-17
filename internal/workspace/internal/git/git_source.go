package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// ResolveSource resolves the committed source context without copying any
// caller changes. baseRef is resolved with --end-of-options; an empty baseRef
// means HEAD.
func (g Git) ResolveSource(ctx context.Context, sourceCwd, baseRef string) (contract.Source, error) {
	if ctx == nil {
		return contract.Source{}, errors.Join(contract.ErrInvalidGitContext, errors.New("nil source context"))
	}
	cwd, err := existingDirectory(sourceCwd)
	if err != nil {
		return contract.Source{}, err
	}
	identity, err := g.readRepositoryIdentity(ctx, cwd)
	if err != nil {
		return contract.Source{}, err
	}
	root, err := existingDirectory(identity.root)
	if err != nil {
		return contract.Source{}, fmt.Errorf("resolve Git root: %w", err)
	}
	gitDir, err := resolveGitDirectory(identity.gitDir, cwd)
	if err != nil {
		return contract.Source{}, fmt.Errorf("resolve Git dir: %w", err)
	}
	commonDir, err := resolveGitDirectory(identity.commonDir, cwd)
	if err != nil {
		return contract.Source{}, fmt.Errorf("resolve Git common dir: %w", err)
	}
	if formatErr := contract.ValidateObjectFormat(identity.objectFormat); formatErr != nil {
		return contract.Source{}, formatErr
	}
	relative, err := relativeRepositoryPath(root, cwd)
	if err != nil {
		return contract.Source{}, err
	}
	if baseRef == "" {
		baseRef = "HEAD"
	}
	if revisionErr := contract.ValidateRevision(baseRef); revisionErr != nil {
		return contract.Source{}, revisionErr
	}
	baseOID, err := g.runScalar(ctx, cwd, []string{"rev-parse", "--verify", "--end-of-options", baseRef + "^{commit}"}, nil)
	if err != nil {
		return contract.Source{}, fmt.Errorf("resolve Git base %q: %w", baseRef, err)
	}
	if oidErr := contract.ValidateOID(baseOID, identity.objectFormat); oidErr != nil {
		return contract.Source{}, fmt.Errorf("resolve Git base %q: %w", baseRef, oidErr)
	}
	dirty, err := g.sourceIsDirty(ctx, cwd)
	if err != nil {
		return contract.Source{}, err
	}
	source := contract.Source{
		Root:         root,
		CommonDir:    commonDir,
		GitDir:       gitDir,
		RelativeCwd:  relative,
		BaseOID:      baseOID,
		ObjectFormat: identity.objectFormat,
		Dirty:        dirty,
	}
	return source, nil
}

type repositoryIdentity struct {
	root         string
	gitDir       string
	commonDir    string
	objectFormat string
}

func (g Git) readRepositoryIdentity(ctx context.Context, directory string) (repositoryIdentity, error) {
	root, err := g.runScalar(ctx, directory, []string{"rev-parse", "--show-toplevel"}, nil)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("read Git root: %w", err)
	}
	gitDir, err := g.runScalar(ctx, directory, []string{"rev-parse", "--git-dir"}, nil)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("read Git dir: %w", err)
	}
	commonDir, err := g.runScalar(ctx, directory, []string{"rev-parse", "--git-common-dir"}, nil)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("read Git common dir: %w", err)
	}
	objectFormat, err := g.runScalar(ctx, directory, []string{"rev-parse", "--show-object-format=storage"}, nil)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("read Git object format: %w", err)
	}
	return repositoryIdentity{root: root, gitDir: gitDir, commonDir: commonDir, objectFormat: objectFormat}, nil
}

func (g Git) sourceIsDirty(ctx context.Context, directory string) (bool, error) {
	presence := &presenceWriter{}
	err := g.run(ctx, directory, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, map[string]string{"GIT_OPTIONAL_LOCKS": "0"}, nil, presence, nil)
	if err != nil {
		return false, fmt.Errorf("observe source dirtiness: %w", err)
	}
	return presence.seen, nil
}

func validateSource(source contract.Source) error {
	if err := contract.ValidateSourceShape(source); err != nil {
		return err
	}
	return validateSourceDirectories(source)
}

func validateSourceDirectories(source contract.Source) error {
	if _, err := existingDirectory(source.Root); err != nil {
		return fmt.Errorf("validate Git root: %w", err)
	}
	if _, err := existingDirectory(source.CommonDir); err != nil {
		return fmt.Errorf("validate Git common dir: %w", err)
	}
	if _, err := existingDirectory(source.GitDir); err != nil {
		return fmt.Errorf("validate Git dir: %w", err)
	}
	return nil
}

func (g Git) validateSourceIdentity(ctx context.Context, source contract.Source) error {
	if err := validateSource(source); err != nil {
		return err
	}
	identity, err := g.readRepositoryIdentity(ctx, source.Root)
	if err != nil {
		return err
	}
	root, err := existingDirectory(identity.root)
	if err != nil {
		return err
	}
	gitDir, err := resolveGitDirectory(identity.gitDir, source.Root)
	if err != nil {
		return err
	}
	commonDir, err := resolveGitDirectory(identity.commonDir, source.Root)
	if err != nil {
		return err
	}
	if root != source.Root || gitDir != source.GitDir || commonDir != source.CommonDir || identity.objectFormat != source.ObjectFormat {
		return fmt.Errorf("%w: source repository changed", contract.ErrIdentityMismatch)
	}
	return nil
}

func existingDirectory(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", errors.Join(contract.ErrInvalidGitContext, errors.New("invalid directory path"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make directory path absolute: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve directory path: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat directory path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", contract.ErrInvalidGitContext, canonical)
	}
	return filepath.Clean(canonical), nil
}

func createPath(path string) (string, error) {
	canonical, err := canonicalPath(path, "checkout path")
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(canonical); err == nil {
		return "", fmt.Errorf("%w: checkout path already exists", contract.ErrInvalidGitContext)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect checkout path: %w", err)
	}
	return filepath.Clean(canonical), nil
}

func canonicalPath(path, label string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("%w: invalid %s", contract.ErrInvalidGitContext, label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make %s absolute: %w", label, err)
	}
	parent, err := existingDirectory(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("validate %s parent: %w", label, err)
	}
	return filepath.Clean(filepath.Join(parent, filepath.Base(abs))), nil
}

func resolveGitDirectory(value, base string) (string, error) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return existingDirectory(path)
}

func relativeRepositoryPath(root, cwd string) (string, error) {
	relative, err := filepath.Rel(root, cwd)
	if err != nil {
		return "", fmt.Errorf("compute source relative cwd: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("%w: source cwd is outside Git root", contract.ErrIdentityMismatch)
	}
	if relative == "." {
		return ".", nil
	}
	return filepath.ToSlash(relative), nil
}
