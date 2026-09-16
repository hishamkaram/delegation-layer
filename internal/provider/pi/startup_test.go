package pi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateStartupMigrationsRejectsProjectCommandsRename(t *testing.T) {
	workspace := t.TempDir()
	projectPi := filepath.Join(workspace, ".pi")
	if err := os.MkdirAll(filepath.Join(projectPi, "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(t.TempDir(), "agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateStartupMigrations(workspace, agentDir); err == nil {
		t.Fatal("migration-triggering project state was accepted")
	}
}

func TestValidateStartupMigrationsAllowsAlreadyMigratedProject(t *testing.T) {
	workspace := t.TempDir()
	projectPi := filepath.Join(workspace, ".pi")
	if err := os.MkdirAll(filepath.Join(projectPi, "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(projectPi, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(t.TempDir(), "agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateStartupMigrations(workspace, agentDir); err != nil {
		t.Fatalf("already migrated project rejected: %v", err)
	}
}

func TestValidateStartupMigrationsRejectsSymlinkedMigrationState(t *testing.T) {
	workspace := t.TempDir()
	projectPi := filepath.Join(workspace, ".pi")
	if err := os.MkdirAll(projectPi, 0o700); err != nil {
		t.Fatal(err)
	}
	commandsTarget := filepath.Join(t.TempDir(), "commands")
	if err := os.Mkdir(commandsTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(commandsTarget, filepath.Join(projectPi, "commands")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(projectPi, "missing-prompts"), filepath.Join(projectPi, "prompts")); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(t.TempDir(), "agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateStartupMigrations(workspace, agentDir); err == nil {
		t.Fatal("symlinked migration state was accepted")
	}
}

func TestValidateStartupMigrationsRejectsAgentDirectoryOverlap(t *testing.T) {
	workspace := t.TempDir()
	agentDir := filepath.Join(workspace, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateStartupMigrations(workspace, agentDir); err == nil {
		t.Fatal("agent directory overlapping workspace was accepted")
	}
}

func TestValidateStartupMigrationsRejectsSessionDirectoryOverlap(t *testing.T) {
	workspace := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(workspace, ".pi", "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateStartupMigrations(workspace, agentDir, sessionDir); err == nil {
		t.Fatal("session directory overlapping workspace was accepted")
	}
}
