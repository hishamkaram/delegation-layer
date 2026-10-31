// Package verifiedexec binds a child process to the executable observed during
// admission. It contains no provider policy or task lifecycle authority.
package verifiedexec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"golang.org/x/sys/unix"
)

const maxVerifiedShebangBytes = 4096

// errViewNoExec reports a view directory on a noexec filesystem where a copied
// executable would have to run.
var errViewNoExec = errors.New("verified executable view directory is on a noexec filesystem")

// Info is the path and content identity of an executable.
type Info struct {
	Path   string
	SHA256 string
}

// Command owns the private materialization of one verified command. Every
// constructor binds the child to a private view of the admitted bytes, never to
// the original path or descriptor, on every supported OS.
type Command struct {
	Cmd            *exec.Cmd
	temporaryDir   string
	temporaryFiles []string
}

// ReleaseDescriptors is the hook callers run after a child has started. A
// verified command holds no inherited descriptors, so there is nothing to
// release; its private view stays owned by Close until the child has exited.
func (c *Command) ReleaseDescriptors() error { return nil }

// Close removes the command's private view after the child has exited.
func (c *Command) Close() error {
	if c == nil {
		return nil
	}
	return c.removeView()
}

// removeView unseals and deletes the command's own view directory.
func (c *Command) removeView() error {
	closeErr := unsealTemporaryDirectory(c.temporaryDir)
	for _, path := range c.temporaryFiles {
		if path != "" {
			closeErr = errors.Join(closeErr, os.RemoveAll(path))
		}
	}
	if c.temporaryDir != "" {
		closeErr = errors.Join(closeErr, os.RemoveAll(c.temporaryDir))
	}
	return closeErr
}

// LocatePath applies the static executable checks used before a supervised
// capability probe. It rejects aliases, special files, and non-executable
// paths while never invoking the provider.
func LocatePath(path string) (Info, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Info{}, fmt.Errorf("resolve provider executable: %w", err)
	}
	canonical, err := config.CanonicalizePath(absolute)
	if err != nil {
		return Info{}, fmt.Errorf("resolve provider executable: %w", err)
	}
	digest, err := Fingerprint(canonical)
	if err != nil {
		return Info{}, fmt.Errorf("inspect provider executable: %w", err)
	}
	return Info{Path: canonical, SHA256: digest}, nil
}

// Fingerprint reads a canonical regular executable without launching it.
func Fingerprint(path string) (digest string, resultErr error) {
	file, before, err := openExecutable(path)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return fingerprintOpened(path, file, before)
}

// Open opens the exact regular executable represented by expectedSHA256 and
// leaves its descriptor positioned at the beginning.
func Open(path, expectedSHA256 string) (*os.File, error) {
	file, before, err := openExecutable(path)
	if err != nil {
		return nil, err
	}
	digest, err := fingerprintOpened(path, file, before)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if digest != expectedSHA256 {
		return nil, errors.Join(changedExecutable(errors.New("runtime executable identity does not match admission")), file.Close())
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// CommandRequest is the immutable-by-convention input of a verified command
// binding: the admitted executable path and digest, the directory used to
// resolve relative PATH entries in a script shebang (matching exec.Cmd.Dir),
// the environment used for interpreter lookup and loader-path policy, and the
// child's arguments. Constructors read the slices only while building the
// command (the exec.Cmd owns its own argument slice), so callers may reuse them
// once the constructor returns.
type CommandRequest struct {
	Executable  string
	SHA256      string
	Directory   string
	Environment []string
	Args        []string
}

// NewCommand binds the inspected executable to a private view of its admitted
// bytes on Linux and macOS alike. A supported interpreter script keeps the
// original entrypoint path in its argument view. Replacing, truncating or
// rewriting the named path after this function returns cannot change the bytes
// the child observes. The caller closes the command after Start/Wait.
func NewCommand(executable, digest string, environment []string, args ...string) (*Command, error) {
	return NewCommandInDirectory(executable, digest, "", environment, args...)
}

// NewCommandInDirectory is NewCommand with the directory that will be used
// for the child launch. Relative PATH entries in a script shebang are resolved
// against that directory, matching exec.Cmd.Dir rather than this process's
// current directory.
func NewCommandInDirectory(executable, digest, directory string, environment []string, args ...string) (*Command, error) {
	return NewCommandFromRequest(CommandRequest{Executable: executable, SHA256: digest, Directory: directory, Environment: environment, Args: args})
}

// NewCommandFromRequest binds the admitted helper bytes to a private view.
func NewCommandFromRequest(request CommandRequest) (*Command, error) {
	file, err := openAdmittedEntrypoint(request)
	if err != nil {
		return nil, err
	}
	return request.privateBinding().newCommand(file)
}

// portableBinding is the named input of the view builder: what is bound, where
// its per-launch view directory is created, and how files are materialized.
type portableBinding struct {
	executable  string
	digest      string
	directory   string
	environment []string
	args        []string
	viewRoot    string
}

func privateBinding(executable, digest, directory string, environment, args []string) portableBinding {
	return portableBinding{
		executable: executable, digest: digest, directory: directory,
		environment: environment, args: args,
		viewRoot: environmentTemporaryRoot(environment),
	}
}

func (r CommandRequest) privateBinding() portableBinding {
	return privateBinding(r.Executable, r.SHA256, r.Directory, r.Environment, r.Args)
}

// newCommand materializes the inspected bytes in a private view directory.
// On macOS, sealed system executables stay at their canonical path because the
// kernel can reject a relocated platform binary even when its bytes match.
// Script module lookups and compiled loader-relative dependencies are backed by
// a private shadow path whose non-entrypoint entries reference the original
// provider installation. The materialized tree is sealed read-only before the
// command is returned, binding its path to the admitted bytes through Start.
// A private view is unsealed and removed when the caller closes the command
// after Start/Wait.
func (b portableBinding) newCommand(file *os.File) (command *Command, resultErr error) {
	viewRoot, err := b.absoluteViewRoot()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	temporaryDir, err := os.MkdirTemp(viewRoot, "delegation-layer-verified-")
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	temporaryFiles := []string(nil)
	sourceClosed := false
	closeSource := func() error {
		if sourceClosed {
			return nil
		}
		sourceClosed = true
		return file.Close()
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeSource(), unsealTemporaryDirectory(temporaryDir), removeTemporaryFiles(temporaryFiles), os.RemoveAll(temporaryDir))
		}
	}()

	cmd, temporaryFiles, err := b.buildCommand(file, temporaryDir)
	if err != nil {
		return nil, err
	}
	if err = sealTemporaryDirectory(temporaryDir); err != nil {
		return nil, err
	}
	if err = closeSource(); err != nil {
		return nil, err
	}
	return &Command{Cmd: cmd, temporaryDir: temporaryDir, temporaryFiles: temporaryFiles}, nil
}

// absoluteViewRoot addresses the view root absolutely, resolved as the child
// resolves a relative TMPDIR, so the view cannot be found against a different
// working directory at Start. The environment itself is not modified.
func (b portableBinding) absoluteViewRoot() (string, error) {
	return launchPath(b.directory, b.viewRoot)
}

// temporaryBoundary is the user's TMPDIR as the child resolves it. It is derived
// from the launch environment.
func (b portableBinding) temporaryBoundary() (string, error) {
	return launchPath(b.directory, environmentTemporaryRoot(b.environment))
}

// buildCommand materializes the entrypoint exactly once and classifies the
// materialized bytes, so the snapshot that is parsed is the snapshot that
// executes. A cache hit therefore cannot turn a rewritten original into a
// different class than the bytes the kernel will see. Protected Darwin system
// executables are the exception: their original sits on the sealed system
// volume, cannot be rewritten, and must keep its canonical path.
func (b portableBinding) buildCommand(file *os.File, temporaryDir string) (*exec.Cmd, []string, error) {
	path, err := b.entrypointPath(temporaryDir)
	if err != nil {
		return nil, nil, err
	}
	if useCanonicalDarwinSystemExecutable(b.executable) {
		return b.buildProtectedCommand(file, path, temporaryDir)
	}
	if err = materializeVerifiedFile(file, path, b.digest); err != nil {
		return nil, nil, err
	}
	shebang, err := readScriptHeader(path)
	if err != nil {
		return nil, nil, err
	}
	if len(shebang) != 0 {
		return b.buildScriptCommand(path, shebang, temporaryDir)
	}
	if err = b.requireExecutableView(temporaryDir); err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(path, b.args...)
	cmd.Args[0] = b.executable
	return cmd, nil, nil
}

// buildProtectedCommand serves a Darwin sealed-system entrypoint. A native one
// stays at its canonical path behind a symlink; a script is copied as data.
func (b portableBinding) buildProtectedCommand(file *os.File, path, temporaryDir string) (*exec.Cmd, []string, error) {
	shebang, err := readShebang(file)
	if err != nil {
		return nil, nil, err
	}
	if len(shebang) == 0 {
		if err = os.Symlink(b.executable, path); err != nil {
			return nil, nil, err
		}
		cmd := exec.Command(path, b.args...)
		cmd.Args[0] = b.executable
		return cmd, []string{path}, nil
	}
	if err = materializeVerifiedFile(file, path, b.digest); err != nil {
		return nil, nil, err
	}
	return b.buildScriptCommand(path, shebang, temporaryDir)
}

// entrypointPath creates the shadow layout of the entrypoint's installation
// directory under the view's "executable" root and returns where the
// entrypoint itself belongs.
func (b portableBinding) entrypointPath(temporaryDir string) (string, error) {
	shadowRoot, err := newPortableShadowRoot(temporaryDir, "executable")
	if err != nil {
		return "", err
	}
	boundary, err := b.temporaryBoundary()
	if err != nil {
		return "", err
	}
	moduleDirectory, err := shadowDirectory(filepath.Dir(b.executable), shadowRoot, filepath.Base(b.executable), boundary)
	if err != nil {
		return "", err
	}
	return filepath.Join(moduleDirectory, filepath.Base(b.executable)), nil
}

// requireExecutableView refuses a view directory on a noexec filesystem. It is
// called only where a copied executable would itself run, never for data that is
// only read or for protected Darwin executables that stay at their canonical
// path. Nothing is left behind: the caller's deferred cleanup removes the view.
func (b portableBinding) requireExecutableView(temporaryDir string) error {
	forbids, err := filesystemForbidsExec(temporaryDir)
	if err != nil {
		return fmt.Errorf("check the view filesystem of %s: %w", temporaryDir, err)
	}
	if !forbids {
		return nil
	}
	return fmt.Errorf("%w: %s (chosen by %s) cannot run the verified copy of %s; the failed view is removed; select an exec-capable directory",
		errViewNoExec, b.viewRoot, b.viewSource(), b.executable)
}

func (b portableBinding) viewSource() string {
	switch {
	case EnvironmentValue(b.environment, "TMPDIR") != "":
		return "TMPDIR in the launch environment"
	default:
		return "the default /tmp"
	}
}

func useCanonicalDarwinSystemExecutable(path string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	path = filepath.Clean(path)
	// The writable Data volume is mounted below /System/Volumes/Data and can
	// contain ordinary user-controlled executables. Only paths in the sealed
	// system volume may use the canonical-path execution required by Darwin's
	// platform code-signature policy.
	if strings.HasPrefix(path, "/System/Volumes/") {
		return false
	}
	for _, prefix := range []string{"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/System/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// buildScriptCommand binds a script whose bytes are already materialized at
// scriptPath and whose shebang was read from those bytes. The interpreter is
// bound too, and a nested script interpreter is refused on the bound
// interpreter, not on a path that can change after it was hashed.
func (b portableBinding) buildScriptCommand(scriptPath string, shebang []string, temporaryDir string) (*exec.Cmd, []string, error) {
	temporaryFiles := []string{scriptPath}
	interpreter, interpreterArgs, kind, err := locateInterpreter(shebang, b.environment, b.directory)
	if err != nil {
		return nil, temporaryFiles, err
	}
	interpreterFile, err := Open(interpreter.Path, interpreter.SHA256)
	if err != nil {
		return nil, temporaryFiles, err
	}
	interpreterPath := interpreter.Path
	interpreterFiles := []string(nil)
	// macOS can terminate a copied system Mach-O interpreter because its
	// platform code signature no longer matches the materialized path. Keep
	// sealed system interpreters canonical; all other interpreters retain the
	// private materialization and its verified digest.
	if !useCanonicalDarwinSystemExecutable(interpreter.Path) {
		if err = b.requireExecutableView(temporaryDir); err == nil {
			interpreterPath, interpreterFiles, err = b.materializeInterpreter(interpreterFile, interpreter, temporaryDir)
		}
	}
	closeErr := interpreterFile.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return nil, append(temporaryFiles, interpreterFiles...), err
	}
	temporaryFiles = append(temporaryFiles, interpreterFiles...)
	if nested, nestedErr := readScriptHeader(interpreterPath); nestedErr != nil {
		return nil, temporaryFiles, nestedErr
	} else if len(nested) != 0 {
		return nil, temporaryFiles, errors.New("nested verified script interpreters are unsupported")
	}
	commandArgs, err := portableScriptArgs(kind, interpreterArgs, scriptPath, b.executable, b.args)
	if err != nil {
		return nil, temporaryFiles, err
	}
	cmd := exec.Command(interpreterPath, commandArgs...)
	cmd.Args[0] = interpreter.Path
	return cmd, temporaryFiles, nil
}

func portableScriptArgs(kind scriptKind, interpreterArgs []string, scriptPath, executable string, args []string) ([]string, error) {
	commandArgs := append([]string(nil), interpreterArgs...)
	switch kind {
	case nodeScript:
		scriptURL := (&url.URL{Scheme: "file", Path: scriptPath}).String()
		commandArgs = append(commandArgs, "--input-type=module", "-e", "import("+strconv.Quote(scriptURL)+")", executable)
	case shellScript:
		commandArgs = append(commandArgs, "-c", `script=$1; shift; . "$script"`, executable, scriptPath)
	default:
		return nil, errors.New("unsupported verified script interpreter")
	}
	return append(commandArgs, args...), nil
}

// shadowDirectory creates a private path matching sourceDirectory. Entries in
// the executable directory and each provider-path ancestor are symlinked to
// the original installation so arbitrarily nested parent-relative imports and
// loader paths such as @loader_path/../lib retain their provider-owned
// semantics. Shared temporary and system roots are deliberately not
// enumerated: their contents are unrelated to the provider and can be very
// large. The admitted entrypoint itself is materialized separately in this
// private path.
func shadowDirectory(sourceDirectory, temporaryDir, excludedEntry, temporaryRoot string) (string, error) {
	relative, err := filepath.Rel(string(filepath.Separator), sourceDirectory)
	if err != nil {
		return "", err
	}
	mirror := temporaryDir
	source := string(filepath.Separator)
	components := []string(nil)
	if relative != "." {
		components = strings.Split(relative, string(filepath.Separator))
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("invalid portable executable path")
		}
		source = filepath.Join(source, component)
		mirror = filepath.Join(mirror, component)
		if err := os.Mkdir(mirror, 0o700); err != nil {
			return "", err
		}
	}
	if err := shadowPathEntries(source, mirror, excludedEntry, temporaryRoot); err != nil {
		return "", err
	}
	return mirror, nil
}

func shadowPathEntries(sourceDirectory, mirrorDirectory, excludedEntry, temporaryRoot string) error {
	source := sourceDirectory
	mirror := mirrorDirectory
	excluded := excludedEntry
	for {
		if err := shadowEntries(source, mirror, excluded); err != nil {
			return err
		}
		parentSource := filepath.Dir(source)
		if parentSource == source || !shouldMirrorParent(parentSource, temporaryRoot) {
			return nil
		}
		excluded = filepath.Base(source)
		source = parentSource
		mirror = filepath.Dir(mirror)
	}
}

func newPortableShadowRoot(temporaryDir, name string) (string, error) {
	root := filepath.Join(temporaryDir, name)
	if err := os.Mkdir(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

func shadowEntries(sourceDirectory, mirrorDirectory, excludedEntry string) error {
	entries, err := os.ReadDir(sourceDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == excludedEntry {
			continue
		}
		if err := os.Symlink(filepath.Join(sourceDirectory, entry.Name()), filepath.Join(mirrorDirectory, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func materializeVerifiedFile(source *os.File, destination, expectedSHA256 string) (resultErr error) {
	if source == nil {
		return errors.New("nil verified executable")
	}
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(destination))
		}
	}()
	return copyVerifiedFile(target, source, expectedSHA256)
}

func (b portableBinding) materializeInterpreter(source *os.File, interpreter Info, temporaryDir string) (path string, temporaryFiles []string, resultErr error) {
	shadowRoot, err := newPortableShadowRoot(temporaryDir, "interpreter")
	if err != nil {
		return "", nil, err
	}
	boundary, err := b.temporaryBoundary()
	if err != nil {
		return "", nil, err
	}
	moduleDirectory, err := shadowDirectory(filepath.Dir(interpreter.Path), shadowRoot, filepath.Base(interpreter.Path), boundary)
	if err != nil {
		return "", nil, err
	}
	path = filepath.Join(moduleDirectory, filepath.Base(interpreter.Path))
	if err := materializeVerifiedFile(source, path, interpreter.SHA256); err != nil {
		return "", nil, err
	}
	return path, nil, nil
}

func sealTemporaryDirectory(path string) error {
	return filepath.WalkDir(path, func(currentPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return os.Chmod(currentPath, 0o500)
	})
}

func unsealTemporaryDirectory(path string) error {
	if path == "" {
		return nil
	}
	err := filepath.WalkDir(path, func(currentPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return nil
		}
		return os.Chmod(currentPath, 0o700)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func copyVerifiedFile(target, source *os.File, expectedSHA256 string) (resultErr error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(target, digest), source)
	closeErr := target.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != expectedSHA256 {
		return errors.New("verified executable snapshot does not match admission")
	}
	return nil
}

func removeTemporaryFiles(paths []string) error {
	var resultErr error
	for _, path := range paths {
		if path != "" {
			resultErr = errors.Join(resultErr, os.RemoveAll(path))
		}
	}
	return resultErr
}

type scriptKind uint8

const (
	nodeScript scriptKind = iota + 1
	shellScript
)

func readShebang(file *os.File) ([]string, error) {
	if file == nil {
		return nil, errors.New("nil verified executable")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	prefix, readErr := io.ReadAll(io.LimitReader(file, maxVerifiedShebangBytes))
	resetErr := func() error {
		_, err := file.Seek(0, io.SeekStart)
		return err
	}()
	if err := errors.Join(readErr, resetErr); err != nil {
		return nil, err
	}
	if len(prefix) < 2 || !bytes.Equal(prefix[:2], []byte("#!")) {
		return nil, nil
	}
	line := prefix[2:]
	if newline := bytes.IndexByte(line, '\n'); newline >= 0 {
		line = line[:newline]
	} else if len(prefix) == maxVerifiedShebangBytes {
		return nil, errors.New("verified script shebang is too long")
	}
	fields := strings.Fields(string(line))
	if len(fields) == 0 {
		return nil, errors.New("verified script shebang is empty")
	}
	return fields, nil
}

func locateInterpreter(shebang, environment []string, directory string) (Info, []string, scriptKind, error) {
	interpreterName := shebang[0]
	interpreterArgs := append([]string(nil), shebang[1:]...)
	if filepath.Base(interpreterName) == "env" {
		if len(interpreterArgs) == 0 {
			return Info{}, nil, 0, errors.New("verified script env interpreter is missing")
		}
		if interpreterArgs[0] == "-S" {
			interpreterArgs = interpreterArgs[1:]
		}
		for len(interpreterArgs) > 0 && strings.Contains(interpreterArgs[0], "=") {
			interpreterArgs = interpreterArgs[1:]
		}
		if len(interpreterArgs) == 0 {
			return Info{}, nil, 0, errors.New("verified script env interpreter is missing")
		}
		interpreterName = interpreterArgs[0]
		interpreterArgs = interpreterArgs[1:]
	}
	if strings.HasPrefix(interpreterName, "-") {
		return Info{}, nil, 0, fmt.Errorf("unsupported verified script interpreter %q", interpreterName)
	}
	interpreterPath, err := locateInEnvironment(interpreterName, environment, directory)
	if err != nil {
		return Info{}, nil, 0, err
	}
	interpreterPath, err = config.CanonicalizePath(interpreterPath)
	if err != nil {
		return Info{}, nil, 0, err
	}
	info, err := LocatePath(interpreterPath)
	if err != nil {
		return Info{}, nil, 0, err
	}

	switch filepath.Base(interpreterName) {
	case "node", "nodejs":
		return info, interpreterArgs, nodeScript, nil
	case "bash", "dash", "sh", "zsh":
		return info, interpreterArgs, shellScript, nil
	default:
		return Info{}, nil, 0, fmt.Errorf("unsupported verified script interpreter %q", interpreterName)
	}
}

func readScriptHeader(path string) (header []string, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return readShebang(file)
}

func locateInEnvironment(name string, environment []string, directory string) (string, error) {
	if filepath.IsAbs(name) || strings.ContainsRune(name, filepath.Separator) {
		return name, nil
	}
	if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve verified script interpreter directory: %w", err)
		}
	}
	pathValue := EnvironmentValue(environment, "PATH")
	for _, pathDirectory := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if pathDirectory == "" {
			pathDirectory = "."
		}
		if !filepath.IsAbs(pathDirectory) {
			pathDirectory = filepath.Join(directory, pathDirectory)
		}
		candidate := filepath.Join(pathDirectory, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("verified script interpreter %q is unavailable", name)
}

func openExecutable(path string) (*os.File, os.FileInfo, error) {
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path || !filepath.IsAbs(path) {
		return nil, nil, errors.New("executable path is not canonical")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, missingExecutable(err)
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 {
		return nil, nil, errors.New("runtime is not a regular executable")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, missingExecutable(err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, nil, errors.Join(errors.New("invalid executable descriptor"), unix.Close(fd))
	}
	return file, before, nil
}

func fingerprintOpened(path string, file *os.File, before os.FileInfo) (string, error) {
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm()&0o111 == 0 || !os.SameFile(before, opened) {
		return "", changedExecutable(errors.New("runtime changed before inspection"))
	}
	hash := sha256.New()
	length, err := io.Copy(hash, file)
	if err != nil {
		return "", err
	}
	final, statErr := file.Stat()
	named, nameErr := os.Lstat(path)
	if err = errors.Join(statErr, missingExecutable(nameErr)); err != nil {
		return "", err
	}
	if !named.Mode().IsRegular() || named.Mode().Perm()&0o111 == 0 || !os.SameFile(named, opened) ||
		!os.SameFile(final, opened) || final.Size() != opened.Size() || length != opened.Size() ||
		!final.ModTime().Equal(opened.ModTime()) || !named.ModTime().Equal(opened.ModTime()) {
		return "", changedExecutable(errors.New("runtime changed during inspection"))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
