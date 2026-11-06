// Package sourceinventory lists maintained source paths without architectural policy.
package sourceinventory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Paths returns sorted, unique, absolute paths of one maintained source kind.
// Git supplies tracked and untracked paths, including tests and build-excluded
// sources, while excluding ignored scratch. Generated Go remains included.
// Only the formatter may explicitly allow an empty Go selection.
func Paths(root, kind string, allowEmpty bool) ([]string, error) {
	extension := sourceExtension(kind)
	if extension == "" {
		return nil, fmt.Errorf("unsupported source kind %q: choose go, python, javascript, or shell", kind)
	}
	if allowEmpty && kind != "go" {
		return nil, errors.New("--allow-empty is only supported for Go formatting")
	}
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	names, err := maintainedNames(canonical)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, name := range names {
		if filepath.Ext(name) != extension {
			continue
		}
		path := filepath.Join(canonical, filepath.FromSlash(name))
		info, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue // A tracked removal is absent from the candidate snapshot.
		}
		if statErr != nil {
			return nil, fmt.Errorf("read maintained path %q: %w", name, statErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("maintained source %q must be a regular file", name)
		}
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve maintained source %q: %w", name, resolveErr)
		}
		if resolved != path {
			return nil, fmt.Errorf("maintained source %q must not traverse symbolic links", name)
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 && !allowEmpty {
		return nil, fmt.Errorf("no maintained %s sources found", kind)
	}
	return paths, nil
}

func maintainedNames(root string) ([]string, error) {
	prefix, err := gitOutput(root, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(prefix, []byte{'\n'}) {
		return nil, errors.New("source inventory root must be the repository root")
	}
	output, err := gitOutput(root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, errors.New("git source inventory must be NUL-terminated")
	}
	names := make([]string, 0)
	for _, record := range bytes.Split(output[:len(output)-1], []byte{0}) {
		name := string(record)
		if name == "" || name == "." || filepath.IsAbs(name) || filepath.Clean(name) != name ||
			name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("invalid maintained path %q", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func gitOutput(root string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if diagnostic := bytes.TrimSpace(exitError.Stderr); len(diagnostic) > 0 {
			return nil, fmt.Errorf("inventory maintained paths: %w: %s", err, diagnostic)
		}
	}
	return nil, fmt.Errorf("inventory maintained paths: %w", err)
}

func canonicalRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("source inventory root must not be empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve source inventory root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve source inventory root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("read source inventory root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("source inventory root must be a directory")
	}
	return canonical, nil
}

func sourceExtension(kind string) string {
	switch kind {
	case "go":
		return ".go"
	case "python":
		return ".py"
	case "javascript":
		return ".mjs"
	case "shell":
		return ".sh"
	default:
		return ""
	}
}
