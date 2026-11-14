package execution

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func connectorPathCwdBind(t *testing.T, store *taskdir.Store, request *task.TaskRecord) {
	t.Helper()
	require(t, store.EnableWorkspaceConnectorContext(t.Context(), true, true))
	root, err := store.RootTokenContext(t.Context())
	require(t, err)
	project, err := taskdir.ObservePathProjectContext(t.Context(), request.CanonicalCwd)
	require(t, err)
	request.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project}
}

func TestPathConnectorExecutionSelectsReplacementBeforeChildAcquisition(t *testing.T) {
	for _, phase := range []string{"preflight", "native_start"} {
		t.Run(phase, func(t *testing.T) {
			directory, permit, plan, evidence := connectorCwdTaskBinding(t, true, true)
			original, err := connectorCwdStat(plan.Directory)
			require(t, err)
			swap := &connectorCwdReplacement{phase: phase, path: plan.Directory}
			result := Run(directory, permit, plan, Options{
				Lifetime:  &connectorProjectGateLifetime{runner: plan.Executable},
				Preflight: swap.preflight,
				Hooks:     Hooks{Start: swap.start, Wait: swap.wait},
			})
			require(t, swap.err)
			assertSuccess(t, result)
			if swap.starts.Load() != 1 || swap.waits.Load() != 1 {
				t.Fatalf("path provider Start/Wait = %d/%d, want 1/1", swap.starts.Load(), swap.waits.Load())
			}
			visible, err := connectorCwdStat(plan.Directory)
			require(t, err)
			data, err := os.ReadFile(evidence)
			require(t, err)
			var observed connectorCwdIdentity
			require(t, json.Unmarshal(data, &observed))
			if visible == original || observed != visible {
				t.Fatalf("path child did not select current replacement: original=%+v visible=%+v child=%+v", original, visible, observed)
			}
		})
	}
}

// The provider reports its initial cwd, then blocks on an invocation-owned pipe.
// Capture replaces that pathname before releasing it, proving the replacement
// happens while the actual fixture process is alive rather than after Wait.
func TestPathConnectorRunningCwdHelper(t *testing.T) {
	evidence := os.Getenv("DELEGATE_PATH_CONNECTOR_CWD_EVIDENCE")
	if evidence == "" {
		return
	}
	identity, err := connectorCwdStat(".")
	if err == nil {
		var encoded []byte
		encoded, err = json.Marshal(identity)
		if err == nil {
			_, err = os.Stdout.Write(append(encoded, '\n'))
		}
	}
	if err != nil {
		os.Exit(41)
	}
	release := os.NewFile(3, "path-provider-release")
	var token [1]byte
	_, readErr := io.ReadFull(release, token[:])
	if err = errors.Join(readErr, release.Close()); err != nil || token[0] != 1 {
		os.Exit(42)
	}
	identity, err = connectorCwdStat(".")
	if err == nil {
		var encoded []byte
		encoded, err = json.Marshal(identity)
		if err == nil {
			err = os.WriteFile(evidence, encoded, 0o600)
		}
	}
	if err == nil {
		_, err = io.Copy(os.Stdout, os.Stdin)
	}
	if err != nil {
		os.Exit(43)
	}
	os.Exit(0)
}

type pathConnectorReleaseLifetime struct {
	connectorProjectGateLifetime
	release *os.File
}

func (l *pathConnectorReleaseLifetime) StartProjectCommand(command *exec.Cmd, directory *os.File, launch task.ProjectLaunchContext, start func() error) error {
	command.ExtraFiles = append(command.ExtraFiles, l.release)
	return l.connectorProjectGateLifetime.StartProjectCommand(command, directory, launch, start)
}

type pathConnectorReplacementObserver struct {
	path    string
	release *os.File
	once    sync.Once
	err     error
}

func (o *pathConnectorReplacementObserver) Observe([]byte) {
	o.once.Do(func() {
		o.err = os.Rename(o.path, o.path+"-moved")
		if o.err == nil {
			o.err = os.Mkdir(o.path, 0o700)
		}
		_, releaseErr := o.release.Write([]byte{1})
		o.err = errors.Join(o.err, releaseErr, o.release.Close())
	})
}

func (o *pathConnectorReplacementObserver) Complete() error { return o.err }

func TestPathConnectorExecutionKeepsRunningCwdAndSealsAfterReplacement(t *testing.T) {
	directory, permit, plan, evidence := connectorCwdTaskBinding(t, true, true)
	original, err := connectorCwdStat(plan.Directory)
	require(t, err)
	reader, writer, err := os.Pipe()
	require(t, err)
	t.Cleanup(func() {
		for _, file := range []*os.File{reader, writer} {
			if closeErr := file.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
				t.Error(closeErr)
			}
		}
	})
	plan.Arguments = []string{"-test.run=^TestPathConnectorRunningCwdHelper$"}
	plan.Environment = append(plan.Environment, "DELEGATE_PATH_CONNECTOR_CWD_EVIDENCE="+evidence)
	lifetime := &pathConnectorReleaseLifetime{connectorProjectGateLifetime: connectorProjectGateLifetime{runner: plan.Executable}, release: reader}
	observer := &pathConnectorReplacementObserver{path: plan.Directory, release: writer}
	var ownership connectorCwdReplacement
	result := Run(directory, permit, plan, Options{Lifetime: lifetime, Identity: observer, Hooks: Hooks{Start: ownership.start, Wait: ownership.wait}})
	require(t, observer.err)
	assertSuccess(t, result)
	if ownership.starts.Load() != 1 || ownership.waits.Load() != 1 {
		t.Fatalf("running path provider Start/Wait = %d/%d, want 1/1", ownership.starts.Load(), ownership.waits.Load())
	}
	data, err := os.ReadFile(evidence)
	require(t, err)
	var observed connectorCwdIdentity
	require(t, json.Unmarshal(data, &observed))
	visible, err := connectorCwdStat(plan.Directory)
	require(t, err)
	if observed != original || observed == visible {
		t.Fatalf("running cwd changed with path replacement: initial=%+v running=%+v visible=%+v", original, observed, visible)
	}
	sealed, cleanupErr, err := directory.CollectContext(t.Context(), task.FixturePredicateRef())
	require(t, errors.Join(cleanupErr, err))
	if sealed.Verdict != task.VerdictCommitted {
		t.Fatalf("path replacement prevented durable sealing: %+v", sealed)
	}
}
