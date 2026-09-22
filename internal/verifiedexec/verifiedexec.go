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

const (
	verifiedExecutablePath  = "/dev/fd/3"
	verifiedInterpreterPath = "/dev/fd/4"
	maxVerifiedShebangBytes = 4096
)

// Info is the path and content identity of an executable.
type Info struct {
	Path   string
	SHA256 string
}

// Command owns the descriptors and temporary materializations used by a
// verified command. On Linux the first descriptor is the admitted entrypoint;
// the second is an interpreter used by a supported script entrypoint.
type Command struct {
	Cmd            *exec.Cmd
	files          []*os.File
	temporaryDir   string
	temporaryFiles []string
}

// ReleaseDescriptors closes parent-owned descriptors after a child has started.
// Portable commands have no inherited descriptors; their private materializations
// remain owned by Close until the child has exited.
func (c *Command) ReleaseDescriptors() error {
	if c == nil {
		return nil
	}
	var closeErr error
	files := c.files
	c.files = nil
	for _, file := range files {
		if file != nil {
			closeErr = errors.Join(closeErr, file.Close())
		}
	}
	return closeErr
}

// Close releases resources owned by a command. A started child has already
// inherited descriptors and opened temporary materializations, so closing and
// removing them does not interrupt that child.
func (c *Command) Close() error {
	if c == nil {
		return nil
	}
	closeErr := c.ReleaseDescriptors()
	closeErr = errors.Join(closeErr, unsealTemporaryDirectory(c.temporaryDir))
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
		return nil, errors.Join(errors.New("runtime executable identity does not match admission"), file.Close())
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// NewCommand keeps the inspected executable bound through native start. Linux
// runs it from an inherited descriptor; other supported platforms use private
// materializations of the admitted bytes. A supported interpreter script keeps
// the original entrypoint path in its argument view. Replacing the named path
// after this function returns cannot change the admitted entrypoint bytes.
func NewCommand(executable, digest string, environment []string, args ...string) (*Command, error) {
	return NewCommandInDirectory(executable, digest, "", environment, args...)
}

// NewCommandInDirectory is NewCommand with the directory that will be used
// for the child launch. Relative PATH entries in a script shebang are resolved
// against that directory, matching exec.Cmd.Dir rather than this process's
// current directory.
func NewCommandInDirectory(executable, digest, directory string, environment []string, args ...string) (*Command, error) {
	file, err := Open(executable, digest)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" {
		return newPortableCommand(executable, digest, directory, environment, args, file)
	}

	shebang, err := readShebang(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if len(shebang) == 0 {
		cmd := exec.Command(verifiedExecutablePath, args...)
		cmd.Args[0] = executable
		cmd.ExtraFiles = []*os.File{file}
		return &Command{Cmd: cmd, files: []*os.File{file}}, nil
	}

	interpreter, interpreterArgs, kind, err := locateInterpreter(shebang, environment, directory)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	interpreterFile, err := Open(interpreter.Path, interpreter.SHA256)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}

	commandArgs := append([]string(nil), interpreterArgs...)
	switch kind {
	case nodeScript:
		commandArgs = append(commandArgs, "--input-type=module", "-e", `import("/dev/fd/3")`, executable)
	case shellScript:
		commandArgs = append(commandArgs, "-c", ". /dev/fd/3", executable)
	default:
		return nil, errors.Join(errors.New("unsupported verified script interpreter"), file.Close(), interpreterFile.Close())
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(verifiedInterpreterPath, commandArgs...)
	cmd.Args[0] = interpreter.Path
	cmd.ExtraFiles = []*os.File{file, interpreterFile}
	return &Command{Cmd: cmd, files: []*os.File{file, interpreterFile}}, nil
}

// newPortableCommand materializes the inspected bytes in a private directory
// on systems where executing an inherited descriptor is not supported. Script
// module lookups and compiled loader-relative dependencies are backed by a
// private shadow path whose non-entrypoint entries reference the original
// provider installation. The materialized tree is sealed read-only before the
// command is returned, binding its path to the admitted bytes through Start.
// The private directory is unsealed and removed when the caller closes the
// command after Start/Wait.
func newPortableCommand(executable, digest, directory string, environment []string, args []string, file *os.File) (command *Command, resultErr error) {
	temporaryDir, err := os.MkdirTemp("", "delegation-layer-verified-")
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

	cmd, temporaryFiles, err := buildPortableCommand(executable, digest, directory, environment, args, file, temporaryDir)
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

func buildPortableCommand(executable, digest, directory string, environment []string, args []string, file *os.File, temporaryDir string) (*exec.Cmd, []string, error) {
	shebang, err := readShebang(file)
	if err != nil {
		return nil, nil, err
	}
	if len(shebang) == 0 {
		shadowRoot, err := newPortableShadowRoot(temporaryDir, "executable")
		if err != nil {
			return nil, nil, err
		}
		moduleDirectory, err := shadowDirectory(filepath.Dir(executable), shadowRoot, filepath.Base(executable))
		if err != nil {
			return nil, nil, err
		}
		path := filepath.Join(moduleDirectory, filepath.Base(executable))
		materializeErr := materializeVerifiedFile(file, path, digest)
		if materializeErr != nil {
			return nil, nil, materializeErr
		}
		cmd := exec.Command(path, args...)
		cmd.Args[0] = executable
		return cmd, nil, nil
	}
	return buildPortableScriptCommand(executable, digest, directory, environment, args, file, temporaryDir, shebang)
}

func buildPortableScriptCommand(executable, digest, directory string, environment []string, args []string, file *os.File, temporaryDir string, shebang []string) (*exec.Cmd, []string, error) {
	scriptPath, err := materializePortableScript(file, executable, temporaryDir, digest)
	if err != nil {
		return nil, nil, err
	}
	temporaryFiles := []string{scriptPath}
	interpreter, interpreterArgs, kind, err := locateInterpreter(shebang, environment, directory)
	if err != nil {
		return nil, temporaryFiles, err
	}
	interpreterFile, err := Open(interpreter.Path, interpreter.SHA256)
	if err != nil {
		return nil, temporaryFiles, err
	}
	interpreterPath, interpreterFiles, materializeErr := materializePortableInterpreter(interpreterFile, interpreter, temporaryDir)
	closeErr := interpreterFile.Close()
	if err = errors.Join(materializeErr, closeErr); err != nil {
		return nil, append(temporaryFiles, interpreterFiles...), err
	}
	temporaryFiles = append(temporaryFiles, interpreterFiles...)
	commandArgs, err := portableScriptArgs(kind, interpreterArgs, scriptPath, executable, args)
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

func materializePortableScript(source *os.File, executable, temporaryDir, expectedSHA256 string) (string, error) {
	shadowRoot, err := newPortableShadowRoot(temporaryDir, "script")
	if err != nil {
		return "", err
	}
	moduleDirectory, err := shadowDirectory(filepath.Dir(executable), shadowRoot, filepath.Base(executable))
	if err != nil {
		return "", err
	}
	path := filepath.Join(moduleDirectory, filepath.Base(executable))
	if err := materializeVerifiedFile(source, path, expectedSHA256); err != nil {
		return "", err
	}
	return path, nil
}

// shadowDirectory creates a private path matching sourceDirectory. Entries
// outside the executable path are symlinked to the original installation so
// relative imports and package lookups retain their provider-owned semantics.
// The admitted entrypoint itself is materialized separately in this private
// path and is never symlinked.
func shadowDirectory(sourceDirectory, temporaryDir, excludedEntry string) (string, error) {
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
		if err := shadowEntries(source, mirror, component); err != nil {
			return "", err
		}
		source = filepath.Join(source, component)
		mirror = filepath.Join(mirror, component)
		if err := os.Mkdir(mirror, 0o700); err != nil {
			return "", err
		}
	}
	if err := shadowEntries(source, mirror, excludedEntry); err != nil {
		return "", err
	}
	return mirror, nil
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

func materializePortableInterpreter(source *os.File, interpreter Info, temporaryDir string) (path string, temporaryFiles []string, resultErr error) {
	shadowRoot, err := newPortableShadowRoot(temporaryDir, "interpreter")
	if err != nil {
		return "", nil, err
	}
	moduleDirectory, err := shadowDirectory(filepath.Dir(interpreter.Path), shadowRoot, filepath.Base(interpreter.Path))
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
	if script, err := readScriptHeader(info.Path); err != nil {
		return Info{}, nil, 0, err
	} else if len(script) != 0 {
		return Info{}, nil, 0, errors.New("nested verified script interpreters are unsupported")
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
	pathValue := ""
	for _, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			pathValue = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	if pathValue == "" {
		pathValue = os.Getenv("PATH")
	}
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
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 {
		return nil, nil, errors.New("runtime is not a regular executable")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
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
		return "", errors.New("runtime changed before inspection")
	}
	hash := sha256.New()
	length, err := io.Copy(hash, file)
	if err != nil {
		return "", err
	}
	final, statErr := file.Stat()
	named, nameErr := os.Lstat(path)
	if err = errors.Join(statErr, nameErr); err != nil {
		return "", err
	}
	if !named.Mode().IsRegular() || named.Mode().Perm()&0o111 == 0 || !os.SameFile(named, opened) ||
		!os.SameFile(final, opened) || final.Size() != opened.Size() || length != opened.Size() ||
		!final.ModTime().Equal(opened.ModTime()) || !named.ModTime().Equal(opened.ModTime()) {
		return "", errors.New("runtime changed during inspection")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
