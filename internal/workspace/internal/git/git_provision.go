package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// Provision creates one detached, locked linked worktree at checkoutPath and
// verifies its registration and admin backlink before returning it. The source
// checkout and its current dirty state are never modified or copied.
func (g Git) Provision(ctx context.Context, source contract.Source, checkoutPath string) (contract.Checkout, error) {
	if ctx == nil {
		return contract.Checkout{}, errors.Join(contract.ErrInvalidGitContext, errors.New("nil provision context"))
	}
	if err := g.validateSourceIdentity(ctx, source); err != nil {
		return contract.Checkout{}, err
	}
	path, err := createPath(checkoutPath)
	if err != nil {
		return contract.Checkout{}, err
	}
	if config.PathsOverlap(path, source.Root) || config.PathsOverlap(path, source.CommonDir) || config.PathsOverlap(path, source.GitDir) {
		return contract.Checkout{}, fmt.Errorf("%w: checkout path overlaps a Git identity path", contract.ErrInvalidGitContext)
	}
	if runErr := g.runWorktreeCommand(ctx, source, []string{"worktree", "add", "--detach", "--lock", path, source.BaseOID}); runErr != nil {
		return contract.Checkout{}, fmt.Errorf("provision Git worktree: %w", runErr)
	}
	checkout, err := g.InspectProvisioned(ctx, source, path)
	if err != nil {
		return contract.Checkout{}, err
	}
	return checkout, nil
}

// InspectProvisioned reconciles an existing detached, locked worktree after a
// caller-owned provision intent survived a crash. It never creates, removes,
// unlocks, or otherwise mutates Git state.
func (g Git) InspectProvisioned(ctx context.Context, source contract.Source, checkoutPath string) (contract.Checkout, error) {
	return g.inspectInitialProvision(ctx, source, checkoutPath, true)
}

// InspectInterruptedProvision recovers initial native ownership without
// requiring the provider's mapped subdirectory to have finished checkout.
// No provider has used a target lacking its durable Checkout record.
func (g Git) InspectInterruptedProvision(ctx context.Context, source contract.Source, checkoutPath string) (contract.Checkout, error) {
	return g.inspectInitialProvision(ctx, source, checkoutPath, false)
}

func (g Git) inspectInitialProvision(ctx context.Context, source contract.Source, path string, requireWorkdir bool) (contract.Checkout, error) {
	checkout, err := g.inspectManagedCheckout(ctx, source, path, requireWorkdir)
	if err != nil {
		return contract.Checkout{}, err
	}
	path = checkout.CheckoutPath
	registration, err := g.checkoutRegistration(ctx, source, path)
	if err != nil {
		return contract.Checkout{}, err
	}
	if !registration.locked {
		return contract.Checkout{}, fmt.Errorf("%w: provisioned checkout is not locked", contract.ErrIdentityMismatch)
	}
	if !strings.EqualFold(registration.head, source.BaseOID) {
		return contract.Checkout{}, fmt.Errorf("%w: registered checkout HEAD is %s, want %s", contract.ErrIdentityMismatch, registration.head, source.BaseOID)
	}
	head, err := g.runScalar(ctx, path, []string{"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"}, nil)
	if err != nil {
		return contract.Checkout{}, fmt.Errorf("inspect provisioned checkout HEAD: %w", err)
	}
	if err = contract.ValidateOID(head, source.ObjectFormat); err != nil {
		return contract.Checkout{}, fmt.Errorf("inspect provisioned checkout HEAD: %w", err)
	}
	if !strings.EqualFold(head, source.BaseOID) {
		return contract.Checkout{}, fmt.Errorf("%w: checkout HEAD is %s, want %s", contract.ErrIdentityMismatch, head, source.BaseOID)
	}
	if err = g.requireDetached(ctx, path); err != nil {
		return contract.Checkout{}, err
	}
	return checkout, nil
}

// InspectCleanup validates the exact native worktree and its bidirectional
// admin backlinks for an authorized cleanup or interrupted provision. It is
// read-only: it never creates, unlocks, removes, prunes, or edits metadata.
// The mapped source-relative workdir may be absent; the checkout root and its
// native registration must still be present and prove the same identity.
func (g Git) InspectCleanup(ctx context.Context, source contract.Source, checkoutPath string) (contract.Checkout, error) {
	return g.inspectManagedCheckout(ctx, source, checkoutPath, false)
}

func (g Git) inspectManagedCheckout(ctx context.Context, source contract.Source, checkoutPath string, requireWorkdir bool) (contract.Checkout, error) {
	if ctx == nil {
		return contract.Checkout{}, errors.Join(contract.ErrInvalidGitContext, errors.New("nil cleanup inspection context"))
	}
	if err := g.validateSourceIdentity(ctx, source); err != nil {
		return contract.Checkout{}, err
	}
	path, err := canonicalPath(checkoutPath, "checkout path")
	if err != nil {
		return contract.Checkout{}, err
	}
	if config.PathsOverlap(path, source.Root) || config.PathsOverlap(path, source.CommonDir) || config.PathsOverlap(path, source.GitDir) {
		return contract.Checkout{}, fmt.Errorf("%w: checkout path overlaps a Git identity path", contract.ErrInvalidGitContext)
	}
	return g.inspectCheckout(ctx, source, path, requireWorkdir)
}

func (g Git) inspectCheckout(ctx context.Context, source contract.Source, path string, requireWorkdir bool) (contract.Checkout, error) {
	checkoutRoot, err := existingDirectory(path)
	if err != nil {
		return contract.Checkout{}, fmt.Errorf("inspect Git checkout: %w", err)
	}
	workdir, err := mappedWorkdirPath(checkoutRoot, source.RelativeCwd)
	if err != nil {
		return contract.Checkout{}, err
	}
	if requireWorkdir {
		workdir, err = existingDirectory(workdir)
		if err != nil {
			return contract.Checkout{}, fmt.Errorf("inspect provisioned checkout workdir: %w", err)
		}
	}
	identity, err := g.readRepositoryIdentity(ctx, checkoutRoot)
	if err != nil {
		return contract.Checkout{}, errors.Join(contract.ErrIdentityMismatch, fmt.Errorf("inspect Git checkout repository: %w", err))
	}
	adminDir, err := resolveGitDirectory(identity.gitDir, checkoutRoot)
	if err != nil {
		return contract.Checkout{}, errors.Join(contract.ErrIdentityMismatch, fmt.Errorf("resolve checkout admin dir: %w", err))
	}
	backlink, err := readAdminBacklink(checkoutRoot, adminDir)
	if err != nil {
		return contract.Checkout{}, err
	}
	if err = readAdminCheckoutBacklink(checkoutRoot, adminDir); err != nil {
		return contract.Checkout{}, err
	}
	checkout := contract.Checkout{
		Source:        source,
		CheckoutPath:  checkoutRoot,
		Workdir:       workdir,
		AdminDir:      adminDir,
		AdminBacklink: backlink,
	}
	if err := g.validateCheckout(ctx, checkout); err != nil {
		return contract.Checkout{}, err
	}
	return checkout, nil
}

// Remove conditionally unlocks and removes exactly the registered checkout
// after validating the source identity, worktree registration, and both admin
// backlinks. It never prunes registrations, renames the checkout, or deletes
// Git metadata directly.
func (g Git) Remove(ctx context.Context, checkout contract.Checkout) error {
	if ctx == nil {
		return errors.Join(contract.ErrInvalidGitContext, errors.New("nil removal context"))
	}
	if err := g.validateCheckout(ctx, checkout); err != nil {
		return err
	}
	registration, err := g.checkoutRegistration(ctx, checkout.Source, checkout.CheckoutPath)
	if err != nil {
		return err
	}
	if registration.locked {
		if unlockErr := g.runWorktreeCommand(ctx, checkout.Source, []string{"worktree", "unlock", checkout.CheckoutPath}); unlockErr != nil {
			return fmt.Errorf("unlock Git worktree: %w", unlockErr)
		}
	}
	if validateErr := g.validateCheckout(ctx, checkout); validateErr != nil {
		return fmt.Errorf("revalidate Git worktree after unlock: %w", validateErr)
	}
	registration, err = g.checkoutRegistration(ctx, checkout.Source, checkout.CheckoutPath)
	if err != nil {
		return fmt.Errorf("revalidate Git worktree registration after unlock: %w", err)
	}
	if registration.locked {
		return fmt.Errorf("%w: Git worktree became locked before removal", contract.ErrIdentityMismatch)
	}
	if err := g.runWorktreeCommand(ctx, checkout.Source, []string{"worktree", "remove", "--force", checkout.CheckoutPath}); err != nil {
		return fmt.Errorf("remove Git worktree: %w", err)
	}
	return g.VerifyRemoved(ctx, checkout)
}

// VerifyRemoved proves both that the exact checkout path is absent and that
// Git no longer reports its exact native worktree registration. It performs
// no cleanup and is intended for a caller with a persisted cleanup intent.
func (g Git) VerifyRemoved(ctx context.Context, checkout contract.Checkout) error {
	if ctx == nil {
		return errors.Join(contract.ErrInvalidGitContext, errors.New("nil removed verification context"))
	}
	if err := g.validateSourceIdentity(ctx, checkout.Source); err != nil {
		return err
	}
	path, err := canonicalPath(checkout.CheckoutPath, "checkout path")
	if err != nil {
		return err
	}
	if path != checkout.CheckoutPath {
		return fmt.Errorf("%w: removed checkout path is not canonical", contract.ErrIdentityMismatch)
	}
	if config.PathsOverlap(path, checkout.Source.Root) || config.PathsOverlap(path, checkout.Source.CommonDir) || config.PathsOverlap(path, checkout.Source.GitDir) {
		return fmt.Errorf("%w: checkout path overlaps a Git identity path", contract.ErrInvalidGitContext)
	}
	if _, err = os.Lstat(path); err == nil {
		return fmt.Errorf("%w: Git worktree path remains after removal", contract.ErrIdentityMismatch)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify removed Git worktree path: %w", err)
	}
	registrations, err := g.worktreeRegistrations(ctx, checkout.Source)
	if err != nil {
		return fmt.Errorf("verify removed Git worktree registration: %w", err)
	}
	_, found, matchErr := matchWorktreeRegistration(registrations, path)
	if matchErr != nil {
		return fmt.Errorf("verify removed Git worktree registration: %w", matchErr)
	}
	if found {
		return fmt.Errorf("%w: Git worktree registration remains after removal", contract.ErrIdentityMismatch)
	}
	return nil
}

func (g Git) validateCheckout(ctx context.Context, checkout contract.Checkout) error {
	if err := g.validateSourceIdentity(ctx, checkout.Source); err != nil {
		return err
	}
	checkoutRoot, err := validateCheckoutPaths(checkout)
	if err != nil {
		return err
	}
	if err = g.validateCheckoutRepository(ctx, checkout, checkoutRoot); err != nil {
		return err
	}
	return g.validateCheckoutRegistration(ctx, checkout, checkoutRoot)
}

func validateCheckoutPaths(checkout contract.Checkout) (string, error) {
	if checkout.CheckoutPath == "" || checkout.Workdir == "" {
		return "", errors.Join(contract.ErrIdentityMismatch, errors.New("checkout path is missing"))
	}
	checkoutRoot, err := existingDirectory(checkout.CheckoutPath)
	if err != nil {
		return "", fmt.Errorf("validate checkout path: %w", err)
	}
	workdir, err := mappedWorkdirPath(checkoutRoot, checkout.Source.RelativeCwd)
	if err != nil {
		return "", err
	}
	declaredWorkdir, err := canonicalPathAllowMissing(checkout.Workdir, "checkout workdir")
	if err != nil {
		return "", fmt.Errorf("validate checkout workdir: %w", err)
	}
	if !contract.PathWithin(checkoutRoot, workdir) || !contract.PathWithin(checkoutRoot, declaredWorkdir) {
		return "", fmt.Errorf("%w: checkout workdir escapes checkout", contract.ErrIdentityMismatch)
	}
	if workdir != declaredWorkdir || checkoutRoot != checkout.CheckoutPath {
		return "", fmt.Errorf("%w: checkout path and workdir differ", contract.ErrIdentityMismatch)
	}
	return checkoutRoot, nil
}

func (g Git) validateCheckoutRepository(ctx context.Context, checkout contract.Checkout, checkoutRoot string) error {
	if checkout.AdminDir == "" || checkout.AdminBacklink == "" {
		return errors.Join(contract.ErrIdentityMismatch, errors.New("checkout admin identity is missing"))
	}
	identity, err := g.readRepositoryIdentity(ctx, checkoutRoot)
	if err != nil {
		return errors.Join(contract.ErrIdentityMismatch, fmt.Errorf("validate checkout repository: %w", err))
	}
	root, err := existingDirectory(identity.root)
	if err != nil {
		return errors.Join(contract.ErrIdentityMismatch, err)
	}
	commonDir, err := resolveGitDirectory(identity.commonDir, checkoutRoot)
	if err != nil {
		return errors.Join(contract.ErrIdentityMismatch, err)
	}
	adminDir, err := resolveGitDirectory(identity.gitDir, checkoutRoot)
	if err != nil {
		return errors.Join(contract.ErrIdentityMismatch, err)
	}
	if root != checkoutRoot || commonDir != checkout.Source.CommonDir || identity.objectFormat != checkout.Source.ObjectFormat {
		return fmt.Errorf("%w: checkout repository does not match source", contract.ErrIdentityMismatch)
	}
	if !contract.PathWithin(checkout.Source.CommonDir, adminDir) {
		return fmt.Errorf("%w: checkout admin directory is outside the source common directory", contract.ErrIdentityMismatch)
	}
	if adminDir != checkout.AdminDir {
		return fmt.Errorf("%w: checkout admin directory changed", contract.ErrIdentityMismatch)
	}
	backlink, err := readAdminBacklink(checkoutRoot, adminDir)
	if err != nil {
		return err
	}
	if backlink != checkout.AdminBacklink {
		return fmt.Errorf("%w: checkout admin backlink changed", contract.ErrIdentityMismatch)
	}
	if err = readAdminCheckoutBacklink(checkoutRoot, adminDir); err != nil {
		return err
	}
	return nil
}

func (g Git) validateCheckoutRegistration(ctx context.Context, checkout contract.Checkout, checkoutRoot string) error {
	registration, err := g.checkoutRegistration(ctx, checkout.Source, checkoutRoot)
	if err != nil {
		return err
	}
	if registration.head == "" {
		return fmt.Errorf("%w: registered checkout HEAD is missing", contract.ErrIdentityMismatch)
	}
	if err = contract.ValidateOID(registration.head, checkout.Source.ObjectFormat); err != nil {
		return fmt.Errorf("invalid registered checkout HEAD: %w", err)
	}
	return nil
}

func (g Git) checkoutRegistration(ctx context.Context, source contract.Source, checkoutRoot string) (worktreeRegistration, error) {
	registrations, err := g.worktreeRegistrations(ctx, source)
	if err != nil {
		return worktreeRegistration{}, err
	}
	registration, found, matchErr := matchWorktreeRegistration(registrations, checkoutRoot)
	if matchErr != nil {
		return worktreeRegistration{}, matchErr
	}
	if !found {
		return worktreeRegistration{}, fmt.Errorf("%w: checkout is not registered", contract.ErrIdentityMismatch)
	}
	return registration, nil
}

func (g Git) requireDetached(ctx context.Context, checkoutRoot string) error {
	_, err := g.runScalar(ctx, checkoutRoot, []string{"symbolic-ref", "-q", "HEAD"}, nil)
	if err == nil {
		return fmt.Errorf("%w: provisioned checkout is attached to a branch", contract.ErrIdentityMismatch)
	}
	var exitCoder gitExitCoder
	if errors.As(err, &exitCoder) && exitCoder.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("inspect provisioned checkout HEAD attachment: %w", err)
}

func mappedWorkdirPath(checkoutRoot, relative string) (string, error) {
	if relative == "." {
		return checkoutRoot, nil
	}
	workdir, err := canonicalPathAllowMissing(filepath.Join(checkoutRoot, filepath.FromSlash(relative)), "mapped checkout cwd")
	if err != nil {
		return "", fmt.Errorf("canonicalize mapped checkout cwd: %w", err)
	}
	if !contract.PathWithin(checkoutRoot, workdir) {
		return "", fmt.Errorf("%w: mapped checkout cwd escapes checkout", contract.ErrIdentityMismatch)
	}
	return workdir, nil
}

func canonicalPathAllowMissing(path, label string) (string, error) {
	return canonicalPathWithMissingSuffix(path, label, existingDirectory)
}

func canonicalPathWithMissingSuffix(path, label string, resolveExisting func(string) (string, error)) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("%w: invalid %s", contract.ErrInvalidGitContext, label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make %s absolute: %w", label, err)
	}
	current, missing, err := existingPathPrefix(filepath.Clean(abs), label)
	if err != nil {
		return "", err
	}
	resolved, err := resolveExisting(current)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	for index := len(missing) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, missing[index])
	}
	return filepath.Clean(resolved), nil
}

func existingPathPrefix(current, label string) (string, []string, error) {
	missing := make([]string, 0, 4)
	for {
		_, statErr := os.Lstat(current)
		if statErr == nil {
			return current, missing, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", nil, fmt.Errorf("inspect %s: %w", label, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil, fmt.Errorf("%w: no existing parent for %s", contract.ErrInvalidGitContext, label)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

type worktreeRegistration struct {
	head   string
	locked bool
}

type worktreeInventoryEntry struct {
	path string
	worktreeRegistration
}

// Inventory parsing preserves raw paths and multiplicity. Filesystem matching
// belongs to the target resolver, not to unrelated worktree validation.
func (g Git) worktreeRegistrations(ctx context.Context, source contract.Source) (registrations []worktreeInventoryEntry, resultErr error) {
	resultErr = g.withWorktreeMetadata(ctx, source, func(bound Git) error {
		_, err := bound.readRecordOutput(ctx, source.Root, []string{"worktree", "list", "--porcelain", "-z"}, func(input io.Reader) error {
			var parseErr error
			registrations, parseErr = parseWorktreeRegistrationsReader(input)
			if parseErr != nil {
				return errors.Join(contract.ErrIdentityMismatch, parseErr)
			}
			return nil
		})
		return err
	})
	return registrations, resultErr
}

func parseWorktreeRegistrations(data []byte) ([]worktreeInventoryEntry, error) {
	return parseWorktreeRegistrationsReader(bytes.NewReader(data))
}

func parseWorktreeRegistrationsReader(reader io.Reader) ([]worktreeInventoryEntry, error) {
	if reader == nil {
		return nil, errors.New("nil Git worktree output")
	}
	registrations := make([]worktreeInventoryEntry, 0)
	var currentPath string
	var current worktreeRegistration
	sawWorktree := false
	flush := func() {
		if currentPath == "" {
			return
		}
		registrations = append(registrations, worktreeInventoryEntry{path: currentPath, worktreeRegistration: current})
		currentPath = ""
		current = worktreeRegistration{}
	}
	buffered := bufio.NewReader(reader)
	for {
		record, readErr := readNULRecord(buffered)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		if len(record) == 0 {
			continue
		}
		recordText := string(record)
		attribute, value, _ := strings.Cut(recordText, " ")
		switch attribute {
		case "worktree":
			flush()
			if !filepath.IsAbs(value) {
				return nil, fmt.Errorf("%w: registered worktree path is not absolute", contract.ErrIdentityMismatch)
			}
			currentPath = value
			sawWorktree = true
		case "HEAD":
			current.head = value
		case "locked":
			current.locked = true
		}
	}
	flush()
	if !sawWorktree || len(registrations) == 0 {
		return nil, errors.New("malformed Git worktree registration output")
	}
	return registrations, nil
}

func canonicalRegisteredPath(path string) (string, error) {
	return canonicalPathWithMissingSuffix(path, "registered worktree path", filepath.EvalSymlinks)
}

func matchWorktreeRegistration(registrations []worktreeInventoryEntry, target string) (worktreeRegistration, bool, error) {
	var matched worktreeRegistration
	found := false
	for _, registration := range registrations {
		path := registration.path
		if path != target {
			var err error
			path, err = canonicalRegisteredPath(path)
			if err != nil {
				return worktreeRegistration{}, false, fmt.Errorf("resolve registered worktree path: %w", errors.Join(contract.ErrIdentityMismatch, err))
			}
		}
		if path != target {
			continue
		}
		if found {
			return worktreeRegistration{}, false, fmt.Errorf("%w: managed checkout has ambiguous Git registrations", contract.ErrIdentityMismatch)
		}
		matched, found = registration.worktreeRegistration, true
	}
	return matched, found, nil
}

func readAdminBacklink(workdir, adminDir string) (string, error) {
	gitPath := filepath.Join(workdir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return "", errors.Join(contract.ErrIdentityMismatch, fmt.Errorf("read checkout .git backlink: %w", err))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: managed checkout .git backlink is not a regular file", contract.ErrIdentityMismatch)
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", errors.Join(contract.ErrIdentityMismatch, fmt.Errorf("read checkout .git backlink: %w", err))
	}
	line := removeOneTerminatingNewline(string(data))
	if !strings.HasPrefix(line, "gitdir: ") || strings.ContainsAny(line[len("gitdir: "):], "\r\n") {
		return "", fmt.Errorf("%w: malformed checkout .git backlink", contract.ErrIdentityMismatch)
	}
	target := line[len("gitdir: "):]
	if target == "" || strings.ContainsRune(target, '\x00') {
		return "", fmt.Errorf("%w: empty checkout .git backlink", contract.ErrIdentityMismatch)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitPath), target)
	}
	canonical, err := existingDirectory(target)
	if err != nil {
		return "", errors.Join(contract.ErrIdentityMismatch, fmt.Errorf("resolve checkout .git backlink: %w", err))
	}
	if canonical != adminDir {
		return "", fmt.Errorf("%w: checkout backlink points to %s, want %s", contract.ErrIdentityMismatch, canonical, adminDir)
	}
	return canonical, nil
}

func readAdminCheckoutBacklink(checkoutRoot, adminDir string) error {
	gitdirPath := filepath.Join(adminDir, "gitdir")
	target, err := readCanonicalRegularFileValue(gitdirPath, "admin gitdir backlink")
	if err != nil {
		return errors.Join(contract.ErrIdentityMismatch, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(adminDir, target)
	}
	canonicalTarget, err := canonicalRegularFile(target, "admin gitdir backlink target")
	if err != nil {
		return errors.Join(contract.ErrIdentityMismatch, err)
	}
	expected, err := canonicalRegularFile(filepath.Join(checkoutRoot, ".git"), "checkout .git backlink")
	if err != nil {
		return errors.Join(contract.ErrIdentityMismatch, err)
	}
	if canonicalTarget != expected {
		return fmt.Errorf("%w: admin gitdir backlink points to %s, want %s", contract.ErrIdentityMismatch, canonicalTarget, expected)
	}
	return nil
}

func readCanonicalRegularFileValue(path, label string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", contract.ErrIdentityMismatch, label)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	value := removeOneTerminatingNewline(string(data))
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%w: malformed %s", contract.ErrIdentityMismatch, label)
	}
	return value, nil
}

func canonicalRegularFile(path, label string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", contract.ErrIdentityMismatch, label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make %s absolute: %w", label, err)
	}
	parent, err := existingDirectory(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve %s parent: %w", label, err)
	}
	return filepath.Clean(filepath.Join(parent, filepath.Base(abs))), nil
}

func readNULRecord(reader *bufio.Reader) ([]byte, error) {
	record, err := reader.ReadBytes(0)
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil, err
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if errors.Is(err, io.EOF) {
			if len(record) == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("malformed NUL-delimited Git output: %w", io.ErrUnexpectedEOF)
		}
		return nil, fmt.Errorf("malformed NUL-delimited Git output: %w", err)
	}
	return record[:len(record)-1], nil
}
