package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestMissingInitialSupervisorConfigUsesPrivateSupervisorPath(t *testing.T) {
	t.Setenv("DELEGATE_PUEUE_CONFIG", "")
	path, err := resolveInitialConfig(Arguments{})
	if err != nil || path != "" {
		t.Fatalf("missing initial config was not left for private supervisor resolution: path=%q err=%v", path, err)
	}
}

func TestExplicitSupervisorConfigCannotClaimPrivateStatePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	_, err := bindInitialWithOptions(Arguments{PueueConfig: pueue.PrivateConfigPath(root)}, Dependencies{}, root, pueue.Options{})
	if !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("private state config was accepted as an explicit supervisor: %v", err)
	}
}

func TestLegacyPrivateBindingWithoutDaemonIdentityRequiresResolvedPair(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	initialExecutable := filepath.Join(bin, "delegate")
	writeRunnerFixture(t, initialExecutable, "#!/bin/sh\nexit 0\n")
	digest := task.ComputeSHA256([]byte("legacy supervisor"))
	saved := task.SupervisorRef{
		ClientExecutable:     "/tmp/pueue",
		ClientSHA256:         digest,
		ResolvedConfigSHA256: digest,
		Endpoint:             "unix:/tmp/pueue.sock",
		ConfigPath:           pueue.PrivateConfigPath(root),
		ConfigDigest:         digest,
		ObservedVersion:      "legacy-pueue",
	}
	if _, err := newSupervisorClient(context.Background(), root, saved, pueue.Options{}, true, initialExecutable); !errors.Is(err, pueue.ErrBinding) {
		t.Fatalf("legacy private binding without a resolvable executable pair was accepted: %v", err)
	}
}

func TestPartialPrivateDaemonIdentityIsRejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	digest := task.ComputeSHA256([]byte("legacy supervisor"))
	saved := task.SupervisorRef{
		ClientExecutable: "/tmp/pueue", ClientSHA256: digest,
		DaemonExecutable: "/tmp/pueued", ResolvedConfigSHA256: digest,
		Endpoint: "unix:/tmp/pueue.sock", ConfigPath: pueue.PrivateConfigPath(root),
		ConfigDigest: digest, ObservedVersion: "legacy-pueue",
	}
	if _, err := newSupervisorClient(context.Background(), root, saved, pueue.Options{}, true, ""); !errors.Is(err, pueue.ErrBinding) {
		t.Fatalf("partial private daemon identity was accepted: %v", err)
	}
}

func TestPrivateRecoveryResolvesPairBesideActiveExecutable(t *testing.T) {
	root := filepath.Join(testsupport.CanonicalTempDir(t), "state")
	bin := filepath.Join(testsupport.CanonicalTempDir(t), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	activeExecutable := filepath.Join(bin, "delegate")
	digest := task.ComputeSHA256([]byte("private supervisor"))
	for _, path := range []string{activeExecutable, filepath.Join(bin, "pueue"), filepath.Join(bin, "pueued")} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldInstall := filepath.Join(testsupport.CanonicalTempDir(t), "removed-install")
	saved := task.SupervisorRef{
		ClientExecutable:     filepath.Join(oldInstall, "pueue"),
		ClientSHA256:         digest,
		DaemonExecutable:     filepath.Join(oldInstall, "pueued"),
		DaemonSHA256:         digest,
		ResolvedConfigSHA256: digest,
		Endpoint:             "unix:" + filepath.Join(root, ".supervisor", "runtime", "pueue.sock"),
		ConfigPath:           pueue.PrivateConfigPath(root),
		ConfigDigest:         digest,
		ObservedVersion:      "pueue 4.0.4",
	}
	emptyPath := testsupport.CanonicalTempDir(t)
	t.Setenv("PATH", emptyPath)
	_, err := newSupervisorClient(context.Background(), root, saved, pueue.Options{}, true, activeExecutable)
	if err == nil || !strings.Contains(err.Error(), "saved private supervisor config is unavailable") {
		t.Fatalf("recovery did not resolve the active executable pair before validating saved state: %v", err)
	}
}

func TestSupervisorOptionsForMetaPreservesSavedEnvironment(t *testing.T) {
	base := pueue.Options{Environment: []string{"HOME=/base", "PATH=/base/bin"}}
	meta := task.MetaRecord{Environment: []string{"HOME=/task", "CODEX_HOME=/task/.codex"}}
	options := supervisorOptionsForMeta(base, meta)
	if !strings.Contains(strings.Join(options.Environment, "\x00"), "HOME=/task") || !strings.Contains(strings.Join(options.Environment, "\x00"), "CODEX_HOME=/task/.codex") {
		t.Fatalf("saved task environment was not carried to supervisor operations: %v", options.Environment)
	}
}

func TestSupervisorOptionsKeepControlConfigWhenProviderIsolatesXDG(t *testing.T) {
	base := pueue.Options{Environment: []string{"HOME=/base", "XDG_CONFIG_HOME=/base/.config"}}
	meta := task.MetaRecord{Environment: []string{"HOME=/task", "XDG_CONFIG_HOME=/task/.config"}}
	options := supervisorOptionsForMeta(base, meta)
	joined := strings.Join(options.Environment, "\x00")
	if !strings.Contains(joined, "XDG_CONFIG_HOME=/base/.config") || strings.Contains(joined, "XDG_CONFIG_HOME=/task/.config") {
		t.Fatalf("provider XDG config replaced supervisor control config: %v", options.Environment)
	}
}

func TestSupervisorOptionsSnapshotPreservesCurrentControlEnvironment(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	}
	options := supervisor.OptionsForCurrentEnvironment(pueue.Options{})
	if options.Environment == nil || !strings.Contains(strings.Join(options.Environment, "\x00"), "HOME=") {
		t.Fatalf("current supervisor environment was not captured: %v", options.Environment)
	}
	if runtime.GOOS == "linux" && !strings.Contains(strings.Join(options.Environment, "\x00"), "XDG_RUNTIME_DIR=") {
		t.Fatalf("Linux XDG runtime selector was not captured: %v", options.Environment)
	}
}

func TestContinuationMetadataRequiresRecordedLaunchState(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	meta := &task.MetaRecord{SupervisorConfig: task.SupervisorRef{ConfigPath: filepath.Join(t.TempDir(), "pueue.yml")}}
	if err := validateContinuationMetadata(root, Arguments{}, meta); err == nil || !strings.Contains(err.Error(), "launch environment") {
		t.Fatalf("missing launch environment was accepted: %v", err)
	}
	meta.Environment = []string{"HOME=/task"}
	if err := validateContinuationMetadata(root, Arguments{}, meta); err == nil || !strings.Contains(err.Error(), "runner executable") {
		t.Fatalf("missing runner was accepted: %v", err)
	}
	meta.RunnerExecutable = "/task/runner"
	if err := validateContinuationMetadata(root, Arguments{Runner: "/other/runner"}, meta); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("runner override error=%v", err)
	}
	if err := validateContinuationMetadata(root, Arguments{PueueConfig: "/other/pueue.yml"}, meta); !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("supervisor override error=%v", err)
	}
	if err := validateContinuationMetadata(root, Arguments{Runner: meta.RunnerExecutable, PueueConfig: meta.SupervisorConfig.ConfigPath}, meta); err != nil {
		t.Fatalf("matching continuation metadata rejected: %v", err)
	}
}

func TestBuildContinuationArgumentsPinsResolvedRoot(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	savedSupervisor := task.SupervisorRef{
		ClientExecutable:     "/task/pueue",
		ClientSHA256:         strings.Repeat("a", 64),
		ResolvedConfigSHA256: strings.Repeat("b", 64),
		Endpoint:             "unix:/task/pueue.sock",
		ConfigPath:           "/task/pueue.yml",
		ConfigDigest:         strings.Repeat("c", 64),
		ObservedVersion:      "task-pueue",
	}
	predecessor := &task.TaskRecord{
		TaskID:       strings.Repeat("a", 32),
		Provider:     config.ProviderFixture,
		CanonicalCwd: "/workspace",
		RequestedConfig: task.TaskConfig{
			Permission: config.ModeReadOnly,
			Budget:     "1m",
		},
	}
	meta := &task.MetaRecord{Environment: []string{"HOME=/task"}, RunnerExecutable: "/task/runner", RunnerOwnership: task.RunnerOwnershipManaged, SupervisorConfig: savedSupervisor}
	arguments, err := buildContinuationArguments(Arguments{}, root, predecessor, meta, []byte("brief"))
	if err != nil {
		t.Fatal(err)
	}
	if arguments.Root != root {
		t.Fatalf("continuation root=%q, want resolved predecessor root %q", arguments.Root, root)
	}
	if arguments.savedSupervisor == nil || *arguments.savedSupervisor != savedSupervisor {
		t.Fatalf("continuation supervisor binding=%+v, want %+v", arguments.savedSupervisor, savedSupervisor)
	}
}

func TestContinuationAdmissionUsesSavedSupervisorBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	digest := strings.Repeat("a", 64)
	saved := task.SupervisorRef{
		ClientExecutable:     "/saved/pueue",
		ClientSHA256:         digest,
		ResolvedConfigSHA256: digest,
		Endpoint:             "unix:/saved/pueue.sock",
		ConfigPath:           filepath.Join(t.TempDir(), "pueue.yml"),
		ConfigDigest:         digest,
		ObservedVersion:      "saved-pueue",
	}
	client, err := bindInitialWithOptions(Arguments{savedSupervisor: &saved}, Dependencies{}, root, pueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Binding(); got != saved {
		t.Fatalf("continuation rebound supervisor: got=%+v want=%+v", got, saved)
	}
}

func TestContinuationClearsAmbientPrivateSupervisorConfig(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	meta := &task.MetaRecord{
		Environment:      []string{"HOME=/task", "PATH=/task/bin", "DELEGATE_PUEUE_CONFIG=/saved/ambient.yml"},
		SupervisorConfig: task.SupervisorRef{ConfigPath: pueue.PrivateConfigPath(root)},
	}
	t.Setenv("DELEGATE_PUEUE_CONFIG", "/ambient/pueue.yml")
	restore, err := applyContinuationEnvironment(root, meta)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DELEGATE_PUEUE_CONFIG"); got != "" {
		t.Fatalf("private continuation retained ambient supervisor config %q", got)
	}
	if got := os.Getenv("HOME"); got != "/task" {
		t.Fatalf("saved continuation environment HOME=%q", got)
	}
	if err = restore(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DELEGATE_PUEUE_CONFIG"); got != "/ambient/pueue.yml" {
		t.Fatalf("ambient supervisor config was not restored: %q", got)
	}
}
