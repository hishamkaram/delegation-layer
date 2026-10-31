package probe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

// probeStartLifetime observes commands at the existing lifetime start boundary.
// Only helpers have an admitted digest and a private view; providers run directly.
type probeStartLifetime struct {
	t          *testing.T
	definition provider.InspectionDefinition
	mu         sync.Mutex
	started    [][]string
}

func (l *probeStartLifetime) StartCommand(cmd *exec.Cmd, start func() error) error {
	definition := l.definition
	if definition.ProviderExecutable() {
		if cmd.Path != definition.Executable || cmd.Args[0] != definition.Executable {
			l.t.Errorf("provider command is not the installed executable: %q %q", cmd.Path, cmd.Args)
		}
	} else {
		assertImmutableVerifiedView(l.t, cmd.Path, definition.Executable, definition.ExecutableSHA256)
		if cmd.Args[0] != definition.Executable {
			l.t.Errorf("helper command lost its admitted argument zero: %q", cmd.Args)
		}
	}
	if cmd.Dir != definition.Directory || !slices.Equal(cmd.Env, definition.Environment) {
		l.t.Errorf("command lost its launch directory or environment: dir=%q env=%q", cmd.Dir, cmd.Env)
	}
	l.mu.Lock()
	l.started = append(l.started, slices.Clone(cmd.Args[1:]))
	l.mu.Unlock()
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

func TestInspectCopiesOnlyNativeHelperCommands(t *testing.T) {
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
// The latter reaches binding directly rather than the inspection fingerprint gate.
func TestInspectRefusesInvalidHelperWithoutStartOrFallback(t *testing.T) {
	for _, fault := range []string{"bad-digest", "missing-helper"} {
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
	if fault == "bad-digest" {
		definition.ExecutableSHA256 = strings.Repeat("0", 64)
	} else {
		definition.Executable = filepath.Join(temporary, "private-missing-helper")
	}
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
	if got := lifetime.arguments(); len(got) != 0 {
		t.Fatalf("refused helper started or fell back: %q", got)
	}
	assertNoVerifiedViews(t, temporary)
}

// binderTemporaryDirectory is the TMPDIR selected for the probe commands. A
// private copy leaves its view here while open; every view must be gone when
// inspection returns.
func binderTemporaryDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directory
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
// integrity-bound helper carrying its admitted digest.
func binderHelperDefinition(t *testing.T, directory, temporary string) provider.InspectionDefinition {
	t.Helper()
	helper := locateTestHelper(t)
	return provider.InspectionDefinition{
		Revision: "native-helper-binder-v1", Executable: helper.Path, ExecutableSHA256: helper.SHA256,
		Arguments: []string{"-c", "printf native"}, Directory: directory,
		Environment: []string{"TMPDIR=" + temporary}, OutputLimit: 64,
		Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"eligible":true}`), nil },
	}
}
