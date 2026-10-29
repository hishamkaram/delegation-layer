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
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// probeTestBinder stands in for an owner-installed binder. It records each
// request and its context, and either refuses the failAt-th request (1-based)
// or binds through a private copy, so its views are observable and removed by
// Command.Close.
type probeTestBinder struct {
	mu       sync.Mutex
	failAt   int
	requests []verifiedexec.CommandRequest
	contexts []context.Context
}

var errProbeTestBinder = errors.New("private-binder-refusal")

func (b *probeTestBinder) BindCommand(ctx context.Context, request verifiedexec.CommandRequest) (*verifiedexec.Command, error) {
	b.mu.Lock()
	b.requests = append(b.requests, request)
	b.contexts = append(b.contexts, ctx)
	refuse := len(b.requests) == b.failAt
	b.mu.Unlock()
	if refuse {
		return nil, errProbeTestBinder
	}
	return verifiedexec.NewCommandFromRequest(request)
}

func (b *probeTestBinder) arguments() [][]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	arguments := make([][]string, 0, len(b.requests))
	for _, request := range b.requests {
		arguments = append(arguments, request.Args)
	}
	return arguments
}

// probeStartLifetime counts the OS starts the scope admits.
type probeStartLifetime struct {
	mu     sync.Mutex
	starts int
}

func (l *probeStartLifetime) StartCommand(_ *exec.Cmd, start func() error) error {
	l.mu.Lock()
	l.starts++
	l.mu.Unlock()
	return start()
}

func (*probeStartLifetime) FinishCommand(context.Context, *exec.Cmd) error { return nil }
func (*probeStartLifetime) Terminate(context.Context) error                { return nil }
func (*probeStartLifetime) CloseTask(context.Context) error                { return nil }

func (l *probeStartLifetime) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts
}

type probeBinderRoute struct {
	name     string
	commands [][]string
	build    func(t *testing.T, directory, temporary string) provider.InspectionDefinition
	failure  error
}

func probeBinderRoutes() []probeBinderRoute {
	return []probeBinderRoute{
		{
			name:     "runtime-and-native-capture",
			commands: [][]string{{"--version"}, {"--help"}, {"native"}},
			build:    binderNativeDefinition,
			failure:  errRuntimeProbe,
		},
		{
			name:     "runtime-and-models",
			commands: [][]string{{"--version"}, {"--help"}, {"--list-models"}},
			build:    binderModelsDefinition,
			failure:  errRuntimeProbe,
		},
	}
}

func TestInspectBindsEveryNativeCommandThroughTheScopeBinder(t *testing.T) {
	for _, route := range probeBinderRoutes() {
		t.Run(route.name, func(t *testing.T) {
			temporary := binderTemporaryDirectory(t)
			definition := route.build(t, t.TempDir(), temporary)
			binder := &probeTestBinder{}
			lifetime := &probeStartLifetime{}
			deadline := time.Now().Add(time.Minute)
			workErr, stopErr := execution.RunSupervised(deadline, execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime, Binder: binder}, func(scope execution.PreflightScope) error {
				_, err := Inspect(scope, definition)
				return err
			})
			if workErr != nil || stopErr != nil {
				t.Fatalf("inspection failed: work=%v stop=%v", workErr, stopErr)
			}
			if got := binder.arguments(); !slices.EqualFunc(got, route.commands, slices.Equal) {
				t.Fatalf("binder requests = %q, want %q", got, route.commands)
			}
			assertScopeBindings(t, binder, definition, deadline)
			if lifetime.count() != len(route.commands) {
				t.Fatalf("starts = %d, want %d", lifetime.count(), len(route.commands))
			}
			assertNoVerifiedViews(t, temporary)
		})
	}
}

// assertScopeBindings checks that every binding carried the launch identity
// and one scope context: the supervised deadline, expired once the scope's
// callback returned.
func assertScopeBindings(t *testing.T, binder *probeTestBinder, definition provider.InspectionDefinition, deadline time.Time) {
	t.Helper()
	for index, request := range binder.requests {
		if request.Executable != definition.Executable || request.SHA256 != definition.ExecutableSHA256 ||
			request.Directory != definition.Directory || !slices.Equal(request.Environment, definition.Environment) {
			t.Fatalf("binder request %d lost the launch identity: %+v", index, request)
		}
		ctx := binder.contexts[index]
		actual, finite := ctx.Deadline()
		if ctx != binder.contexts[0] || !finite || !actual.Equal(deadline) || ctx.Err() == nil {
			t.Fatalf("binder request %d did not receive the expired scope deadline context", index)
		}
	}
}

func TestInspectRefusesSuppliedBinderFaultWithoutStartOrFallback(t *testing.T) {
	for _, route := range probeBinderRoutes() {
		for failAt := 1; failAt <= len(route.commands); failAt++ {
			t.Run(route.name+"/"+strings.Join(route.commands[failAt-1], ""), func(t *testing.T) {
				temporary := binderTemporaryDirectory(t)
				definition := route.build(t, t.TempDir(), temporary)
				binder := &probeTestBinder{failAt: failAt}
				lifetime := &probeStartLifetime{}
				workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime, Binder: binder}, func(scope execution.PreflightScope) error {
					_, err := Inspect(scope, definition)
					return err
				})
				wantFailure := route.failure
				if failAt == len(route.commands) {
					wantFailure = errNativeInspection
				}
				if !errors.Is(workErr, wantFailure) || stopErr != nil || strings.Contains(workErr.Error(), "private-") {
					t.Fatalf("binder refusal result: work=%v stop=%v, want %v", workErr, stopErr, wantFailure)
				}
				if len(binder.requests) != failAt {
					t.Fatalf("binder requests = %d, want %d", len(binder.requests), failAt)
				}
				if lifetime.count() != failAt-1 {
					t.Fatalf("starts = %d, want %d: a refused binding must not start", lifetime.count(), failAt-1)
				}
				assertNoVerifiedViews(t, temporary)
			})
		}
	}
}

// binderTemporaryDirectory is the TMPDIR selected for the probe commands. A
// private copy made outside the supplied binder would leave its view here
// while the command is open; every view must be gone when inspection returns.
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
	script := "#!/bin/sh\nset -eu\ncase \"${1-}\" in\n  --version) printf 'provider 1.0\\n' ;;\n  --help) printf '%s\\n' '--list-models' ;;\n  native) printf 'native' ;;\n  --list-models) ;;\n  *) exit 64 ;;\nesac\n"
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
