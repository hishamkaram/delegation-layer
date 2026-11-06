package probe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

// This observes only finite fixture commands and exact Wait completion. It
// supplies no resident, independent-placement, or provider-account proof.
type finalEnvironmentLifetime struct {
	t        *testing.T
	expected []string
	started  []*exec.Cmd
	finished int
}

func (l *finalEnvironmentLifetime) StartCommand(command *exec.Cmd, start func() error) error {
	if !slices.Equal(command.Env, l.expected) {
		l.t.Error("native command lost final prepared environment")
	}
	err := start()
	if command.Process != nil {
		l.started = append(l.started, command)
	}
	return err
}

func (l *finalEnvironmentLifetime) FinishCommand(_ context.Context, command *exec.Cmd) error {
	if command.ProcessState == nil {
		return errors.New("finite fixture command has no joined Wait")
	}
	l.finished++
	return nil
}

func (*finalEnvironmentLifetime) Terminate(context.Context) error {
	return errors.New("unexpected finite fixture expiry")
}

func (l *finalEnvironmentLifetime) CloseTask(context.Context) error {
	if len(l.started) != l.finished {
		return errors.New("finite fixture retains unjoined commands")
	}
	return nil
}

func finalEnvironmentDirectories(t *testing.T) (directory, temporary string, closed *bool) {
	t.Helper()
	parent := ""
	joined := false
	t.Cleanup(func() {
		if parent == "" {
			return
		}
		if !joined {
			t.Errorf("finite probe fixture closure unresolved; retain %s", parent)
			return
		}
		if err := os.RemoveAll(parent); err != nil {
			t.Errorf("remove finite probe fixture %s: %v", parent, err)
		}
	})
	root, err := os.MkdirTemp("", "dl-probe-env-")
	if err != nil {
		t.Fatal(err)
	}
	parent = root
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	directory, temporary = filepath.Join(canonical, "work"), filepath.Join(canonical, "temporary")
	for _, path := range []string{directory, temporary} {
		if err = os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return directory, temporary, &joined
}

func TestFinalEnvironmentCoversEveryInspectionCommand(t *testing.T) {
	for _, route := range probeBinderRoutes() {
		if !t.Run(route.name, func(t *testing.T) {
			assertFinalInspectionEnvironment(t, route)
		}) {
			t.FailNow()
		}
	}
}

func assertFinalInspectionEnvironment(t *testing.T, route probeBinderRoute) {
	t.Helper()
	directory, temporary, closed := finalEnvironmentDirectories(t)
	definition := route.build(t, directory, temporary)
	definition.Environment = append(definition.Environment, "CUSTOM=selected", "DELEGATION_LAYER_PROCESS_SCOPE=finite-owner")
	if definition.Runtime != nil {
		definition.Runtime.Environment = slices.Clone(definition.Environment)
	}
	_, digest, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	original := slices.Clone(definition.Environment)
	expected := append(slices.Clone(original), "OPERATIONAL=prepared", "RAW=\xff")
	lifetime := &finalEnvironmentLifetime{t: t, expected: expected}
	calls := 0
	var facts []byte
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime, PrepareEnvironment: func(ctx context.Context, selected []string) ([]string, error) {
		calls++
		if ctx == nil || ctx.Err() != nil {
			return nil, context.Canceled
		}
		return append(selected, "OPERATIONAL=prepared", "RAW=\xff"), nil
	}}, func(scope execution.PreflightScope) error {
		result, inspectErr := Inspect(scope, definition)
		facts = result
		return inspectErr
	})
	if workErr != nil || stopErr != nil {
		t.Fatalf("prepared inspection failed: %v %v", workErr, stopErr)
	}
	if len(lifetime.started) != len(route.started) || lifetime.finished != len(route.started) || calls != len(route.started) {
		t.Fatalf("inspection command counts: started=%d waited=%d prepared=%d", len(lifetime.started), lifetime.finished, calls)
	}
	_, afterDigest, err := definition.Snapshot()
	if err != nil || digest != afterDigest || !slices.Equal(original, definition.Environment) {
		t.Fatal("operational environment changed inspection description")
	}
	assertPreparedInspectionFacts(t, definition, facts)
	assertNoVerifiedViews(t, temporary)
	*closed = true
}

func assertPreparedInspectionFacts(t *testing.T, definition provider.InspectionDefinition, facts []byte) {
	t.Helper()
	if definition.Runtime == nil {
		return
	}
	decoded, err := provider.DecodeInspectionFacts(facts)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := provider.RuntimeCapabilityDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Runtime == nil || decoded.Runtime.CapabilitySHA256 != expected {
		t.Fatal("prepared transport changed admitted runtime capability digest")
	}
}

func TestFinalEnvironmentInspectionRefusalPrecedesBindingAndStart(t *testing.T) {
	for _, route := range probeBinderRoutes() {
		if !t.Run(route.name, func(t *testing.T) {
			directory, temporary, closed := finalEnvironmentDirectories(t)
			definition := route.build(t, directory, temporary)
			lifetime := &finalEnvironmentLifetime{t: t}
			calls := 0
			workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime, PrepareEnvironment: func(context.Context, []string) ([]string, error) {
				calls++
				return nil, errors.New("synthetic environment refusal")
			}}, func(scope execution.PreflightScope) error {
				_, err := Inspect(scope, definition)
				return err
			})
			if workErr == nil || stopErr != nil || calls != 1 || len(lifetime.started) != 0 {
				t.Fatalf("environment refusal reached native command: work=%v stop=%v preparations=%d starts=%d", workErr, stopErr, calls, len(lifetime.started))
			}
			assertNoVerifiedViews(t, temporary)
			*closed = true
		}) {
			t.FailNow()
		}
	}
}
