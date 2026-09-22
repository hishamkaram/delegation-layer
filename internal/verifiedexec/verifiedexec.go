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
	"os"
	"os/exec"
	"path/filepath"
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

// Command owns the descriptors inherited by a verified command. The first
// descriptor is the admitted entrypoint; the second is an interpreter used by
// a supported script entrypoint.
type Command struct {
	Cmd   *exec.Cmd
	files []*os.File
}

// Close releases descriptors owned by a command. A started child has already
// inherited its descriptors, so closing them does not interrupt that child.
func (c *Command) Close() error {
	if c == nil {
		return nil
	}
	var closeErr error
	for _, file := range c.files {
		if file != nil {
			closeErr = errors.Join(closeErr, file.Close())
		}
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

// NewCommand keeps the inspected executable open through native start. A
// compiled executable runs directly from its descriptor. A supported
// interpreter script runs from that descriptor through a verified interpreter,
// while retaining the original entrypoint path in the script's argument view.
// Replacing the named path after this function returns cannot change the file
// that executes.
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
