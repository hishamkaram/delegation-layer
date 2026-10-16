package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveSource resolves the committed source context without copying any
// caller changes. baseRef is resolved with --end-of-options; an empty baseRef
// means HEAD.
func (g Git) ResolveSource(ctx context.Context, sourceCwd, baseRef string) (Source, error) {
	if ctx == nil {
		return Source{}, errors.Join(ErrInvalidGitContext, errors.New("nil source context"))
	}
	cwd, err := existingDirectory(sourceCwd)
	if err != nil {
		return Source{}, err
	}
	identity, err := g.readRepositoryIdentity(ctx, cwd)
	if err != nil {
		return Source{}, err
	}
	root, err := existingDirectory(identity.root)
	if err != nil {
		return Source{}, fmt.Errorf("resolve Git root: %w", err)
	}
	gitDir, err := resolveGitDirectory(identity.gitDir, cwd)
	if err != nil {
		return Source{}, fmt.Errorf("resolve Git dir: %w", err)
	}
	commonDir, err := resolveGitDirectory(identity.commonDir, cwd)
	if err != nil {
		return Source{}, fmt.Errorf("resolve Git common dir: %w", err)
	}
	if formatErr := validateObjectFormat(identity.objectFormat); formatErr != nil {
		return Source{}, formatErr
	}
	relative, err := relativeRepositoryPath(root, cwd)
	if err != nil {
		return Source{}, err
	}
	if baseRef == "" {
		baseRef = "HEAD"
	}
	if revisionErr := validateRevision(baseRef); revisionErr != nil {
		return Source{}, revisionErr
	}
	baseOID, err := g.runScalar(ctx, cwd, []string{"rev-parse", "--verify", "--end-of-options", baseRef + "^{commit}"}, nil)
	if err != nil {
		return Source{}, fmt.Errorf("resolve Git base %q: %w", baseRef, err)
	}
	if oidErr := validateOID(baseOID, identity.objectFormat); oidErr != nil {
		return Source{}, fmt.Errorf("resolve Git base %q: %w", baseRef, oidErr)
	}
	dirty, err := g.sourceIsDirty(ctx, cwd)
	if err != nil {
		return Source{}, err
	}
	source := Source{
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

func validateSource(source Source) error {
	if err := validateSourceShape(source); err != nil {
		return err
	}
	return validateSourceDirectories(source)
}

func validateSourceShape(source Source) error {
	if !hasSourceFields(source) {
		return errors.Join(ErrInvalidGitContext, errors.New("incomplete Git source"))
	}
	if err := validateObjectFormat(source.ObjectFormat); err != nil {
		return err
	}
	if err := validateOID(source.BaseOID, source.ObjectFormat); err != nil {
		return fmt.Errorf("invalid source base OID: %w", err)
	}
	return validateSourceRelativeCwd(source.RelativeCwd)
}

func hasSourceFields(source Source) bool {
	return source.Root != "" && source.CommonDir != "" && source.GitDir != "" && source.BaseOID != "" && source.ObjectFormat != ""
}

func validateSourceRelativeCwd(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return errors.Join(ErrInvalidGitContext, errors.New("invalid source relative cwd"))
	}
	relative := filepath.FromSlash(value)
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Clean(relative) != relative {
		return errors.Join(ErrInvalidGitContext, errors.New("invalid source relative cwd"))
	}
	return nil
}

func validateSourceDirectories(source Source) error {
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

func (g Git) validateSourceIdentity(ctx context.Context, source Source) error {
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
		return fmt.Errorf("%w: source repository changed", ErrIdentityMismatch)
	}
	return nil
}

func existingDirectory(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", errors.Join(ErrInvalidGitContext, errors.New("invalid directory path"))
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
		return "", fmt.Errorf("%w: %s is not a directory", ErrInvalidGitContext, canonical)
	}
	return filepath.Clean(canonical), nil
}

func createPath(path string) (string, error) {
	canonical, err := canonicalPath(path, "checkout path")
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(canonical); err == nil {
		return "", fmt.Errorf("%w: checkout path already exists", ErrInvalidGitContext)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect checkout path: %w", err)
	}
	return filepath.Clean(canonical), nil
}

func canonicalPath(path, label string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("%w: invalid %s", ErrInvalidGitContext, label)
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
		return "", fmt.Errorf("%w: source cwd is outside Git root", ErrIdentityMismatch)
	}
	if relative == "." {
		return ".", nil
	}
	return filepath.ToSlash(relative), nil
}

func pathWithin(root, path string) bool {
	if root == path {
		return true
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func validateObjectFormat(format string) error {
	if format != "sha1" && format != "sha256" {
		return fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, format)
	}
	return nil
}

func validateOID(oid, objectFormat string) error {
	if err := validateObjectFormat(objectFormat); err != nil {
		return err
	}
	want := 40
	if objectFormat == "sha256" {
		want = 64
	}
	if len(oid) != want {
		return fmt.Errorf("invalid %s OID length %d", objectFormat, len(oid))
	}
	for _, character := range oid {
		if !isHexDigit(character) {
			return fmt.Errorf("invalid %s OID", objectFormat)
		}
	}
	return nil
}

func isHexDigit(character rune) bool {
	return (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')
}

func validateRevision(revision string) error {
	if revision == "" || strings.ContainsAny(revision, "\x00\r\n") {
		return errors.Join(ErrInvalidGitContext, errors.New("invalid Git revision"))
	}
	return nil
}
