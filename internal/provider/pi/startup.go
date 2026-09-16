package pi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// validateStartupMigrations rejects native Pi startup states that can mutate
// the selected workspace before the CLI reaches --help or emits JSON. Pi's
// migration runner is executed before argument handling, and neither
// --no-extensions nor --offline disables it. The optional session directory
// is checked as well because Pi creates and appends session JSONL there even
// when no migration is pending.
func validateStartupMigrations(workspace, agentDir string, sessionDirs ...string) error {
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || !filepath.IsAbs(agentDir) || filepath.Clean(agentDir) != agentDir {
		return fmt.Errorf("%w: Pi startup paths must be canonical absolute paths", ErrUnsupportedProfile)
	}
	if pathsOverlap(workspace, agentDir) {
		return fmt.Errorf("%w: Pi native agent directory overlaps the task workspace", ErrUnsupportedProfile)
	}
	for _, sessionDir := range sessionDirs {
		if !filepath.IsAbs(sessionDir) || filepath.Clean(sessionDir) != sessionDir {
			return fmt.Errorf("%w: Pi session directory must be a canonical absolute path", ErrUnsupportedProfile)
		}
		if pathsOverlap(workspace, sessionDir) {
			return fmt.Errorf("%w: Pi session directory overlaps the task workspace", ErrUnsupportedProfile)
		}
	}

	projectPi := filepath.Join(workspace, ".pi")
	commands := filepath.Join(projectPi, "commands")
	commandsPresent, err := pathPresent(commands)
	if err != nil {
		return fmt.Errorf("%w: inspect Pi project migrations: %w", ErrUnsupportedProfile, err)
	}
	if !commandsPresent {
		return nil
	}
	promptsPresent, err := pathPresent(filepath.Join(projectPi, "prompts"))
	if err != nil {
		return fmt.Errorf("%w: inspect Pi project migrations: %w", ErrUnsupportedProfile, err)
	}
	if !promptsPresent {
		return fmt.Errorf("%w: Pi would migrate .pi/commands before the task starts", ErrUnsupportedProfile)
	}
	return nil
}

func pathPresent(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("pi startup path cannot be a symlink: %s", path)
	}
	return true, nil
}

func pathsOverlap(left, right string) bool {
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !hasParentPrefix(relative)
}

func hasParentPrefix(relative string) bool {
	return len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator)
}
