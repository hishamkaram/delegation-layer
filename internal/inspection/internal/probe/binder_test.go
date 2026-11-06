package probe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

// probeStartLifetime observes commands at the existing lifetime start boundary.
// Every role runs directly at the selected installed path.
type probeStartLifetime struct {
	t          *testing.T
	definition provider.InspectionDefinition
	mu         sync.Mutex
	started    [][]string
	refusal    error
}

func (l *probeStartLifetime) StartCommand(cmd *exec.Cmd, start func() error) error {
	definition := l.definition
	if cmd.Path != definition.Executable || cmd.Args[0] != definition.Executable {
		l.t.Errorf("command is not the selected installed executable: %q %q", cmd.Path, cmd.Args)
	}
	if cmd.Dir != definition.Directory || !slices.Equal(cmd.Env, definition.Environment) {
		l.t.Errorf("command lost its launch directory or environment: dir=%q env=%q", cmd.Dir, cmd.Env)
	}
	l.mu.Lock()
	l.started = append(l.started, slices.Clone(cmd.Args[1:]))
	l.mu.Unlock()
	if l.refusal != nil {
		return l.refusal
	}
	return start()
}

func (*probeStartLifetime) FinishCommand(context.Context, *exec.Cmd) error { return nil }
func (*probeStartLifetime) Terminate(context.Context) error                { return nil }
func (*probeStartLifetime) CloseTask(context.Context) error                { return nil }

func (l *probeStartLifetime) arguments() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.started)
}

type probeBinderRoute struct {
	name    string
	started [][]string
	build   func(t *testing.T, directory, temporary string) provider.InspectionDefinition
}

func probeBinderRoutes() []probeBinderRoute {
	return []probeBinderRoute{
		{
			name:    "provider-runtime-and-native-capture",
			started: [][]string{{"--help"}, {"native"}},
			build:   binderNativeDefinition,
		},
		{
			name:    "provider-models",
			started: [][]string{{"--help"}, {"--list-models"}},
			build:   binderModelsDefinition,
		},
		{
			name:    "native-helper-capture",
			started: [][]string{{"-c", "printf native"}},
			build:   binderHelperDefinition,
		},
	}
}

func TestInspectRunsSelectedInstalledCommands(t *testing.T) {
	for _, route := range probeBinderRoutes() {
		t.Run(route.name, func(t *testing.T) {
			temporary := binderTemporaryDirectory(t)
			definition := route.build(t, t.TempDir(), temporary)
			lifetime := &probeStartLifetime{t: t, definition: definition}
			deadline := time.Now().Add(time.Minute)
			workErr, stopErr := execution.RunSupervised(deadline, execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime}, func(scope execution.PreflightScope) error {
				_, err := Inspect(scope, definition)
				return err
			})
			if workErr != nil || stopErr != nil {
				t.Fatalf("inspection failed: work=%v stop=%v", workErr, stopErr)
			}
			if got := lifetime.arguments(); !slices.EqualFunc(got, route.started, slices.Equal) {
				t.Fatalf("started arguments = %q, want %q", got, route.started)
			}
			assertNoVerifiedViews(t, temporary)
		})
	}
}

// Both the inspection entrypoint and native capture refuse real helper faults.
// Both retain a single native Start attempt with no fallback.
func TestInspectRefusesInvalidHelperWithoutStartOrFallback(t *testing.T) {
	for _, fault := range []string{"non-executable-helper", "missing-helper"} {
		for _, entry := range []string{"inspect", "native-capture"} {
			t.Run(fault+"/"+entry, func(t *testing.T) {
				assertInvalidHelperRefused(t, fault, entry)
			})
		}
	}
}

func assertInvalidHelperRefused(t *testing.T, fault, entry string) {
	t.Helper()
	temporary := binderTemporaryDirectory(t)
	definition := binderHelperDefinition(t, t.TempDir(), temporary)
	definition.Executable = filepath.Join(temporary, "selected-helper")
	original := prepareInvalidBinderHelper(t, definition.Executable, fault)
	lifetime := &probeStartLifetime{t: t, definition: definition}
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime}, func(scope execution.PreflightScope) error {
		var data []byte
		var err error
		if entry == "inspect" {
			data, err = Inspect(scope, definition)
		} else {
			data, err = runNative(scope, definition, nativeHooks{})
		}
		defer clear(data)
		if data != nil {
			t.Error("refused helper returned inspection data")
		}
		return err
	})
	if !errors.Is(workErr, errNativeInspection) || stopErr != nil || workErr.Error() != errNativeInspection.Error() {
		t.Fatalf("helper refusal result: work=%v stop=%v, want sanitized %v", workErr, stopErr, errNativeInspection)
	}
	if got := lifetime.arguments(); len(got) != 1 || !slices.Equal(got[0], definition.Arguments) {
		t.Fatalf("helper must have exactly one selected-path Start attempt and no fallback: %q", got)
	}
	assertInvalidBinderHelperUnchanged(t, definition.Executable, original)
}

// binderTemporaryDirectory selects an owned TMPDIR. Inspection must leave it
// unchanged because installed commands do not stage executable views.
func binderTemporaryDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func prepareInvalidBinderHelper(t *testing.T, path, fault string) os.FileInfo {
	t.Helper()
	if fault == "missing-helper" {
		return nil
	}
	if fault != "non-executable-helper" {
		t.Fatalf("unknown helper fault %q", fault)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf native\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return original
}

func assertInvalidBinderHelperUnchanged(t *testing.T, path string, original os.FileInfo) {
	t.Helper()
	if original == nil {
		assertNoVerifiedViews(t, filepath.Dir(path))
		return
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("helper refusal added or removed fixture entries: %v", entries)
	}
	current, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, current) || current.Mode() != original.Mode() || current.Size() != original.Size() || !current.ModTime().Equal(original.ModTime()) {
		t.Fatalf("helper refusal changed the original installation metadata: before=%v after=%v", original, current)
	}
}

func assertNoVerifiedViews(t *testing.T, temporary string) {
	t.Helper()
	entries, err := os.ReadDir(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("verified views remain in %s: %v", temporary, entries)
	}
}

func writeBinderCLI(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "provider-cli")
	script := "#!/bin/sh\nset -eu\ncase \"${1-}\" in\n  --help) printf '%s\\n' '--list-models' ;;\n  native) printf 'native' ;;\n  --list-models) ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func binderNativeDefinition(t *testing.T, directory, temporary string) provider.InspectionDefinition {
	t.Helper()
	definition := runtimeDefinition(t, directory, writeBinderCLI(t, directory), nil)
	definition.Revision = "native-binder-v1"
	definition.Environment = []string{"TMPDIR=" + temporary}
	definition.Runtime.Environment = []string{"TMPDIR=" + temporary}
	definition.Arguments = []string{"native"}
	definition.OutputLimit = 64
	definition.Project = func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"eligible":true}`), nil }
	return definition
}

func binderModelsDefinition(t *testing.T, directory, temporary string) provider.InspectionDefinition {
	t.Helper()
	definition := runtimeDefinition(t, directory, writeBinderCLI(t, directory), nil)
	definition.Runtime = nil
	definition.Revision = provider.ModelsRevision
	definition.Environment = []string{"TMPDIR=" + temporary}
	definition.OutputLimit = 64
	definition.Models = &provider.ModelDiscoveryDefinition{
		Arguments:  []string{"--list-models"},
		Source:     "fixture",
		Capability: provider.RuntimeCapability{RequiredFlags: []string{"--list-models"}},
		Project: func([]byte, []byte, []byte, bool) (provider.ModelCatalog, error) {
			return provider.ModelCatalog{}, errors.New("fixture lists no models")
		},
	}
	return definition
}

// binderHelperDefinition is a native-only inspection whose executable is an
// installed helper selected by metadata.
func binderHelperDefinition(t *testing.T, directory, temporary string) provider.InspectionDefinition {
	t.Helper()
	helper := locateTestHelper(t)
	return provider.InspectionDefinition{
		Revision: "native-helper-binder-v1", Executable: helper.Path,
		Arguments: []string{"-c", "printf native"}, Directory: directory,
		Environment: []string{"TMPDIR=" + temporary}, OutputLimit: 64,
		Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"eligible":true}`), nil },
	}
}
