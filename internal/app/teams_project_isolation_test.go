package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type teamsIsolationPIDs struct {
	Parent int `json:"parent"`
	Child  int `json:"child"`
}

// This probe owns a child running the existing stdin-controlled fixture. The
// child must acknowledge its blocking read before either PID is published.
func TestTeamsProjectIsolationDescendantHelper(t *testing.T) {
	if os.Getenv("DELEGATE_TEAMS_ISOLATION_DESCENDANT") != "1" {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestTeamsProjectRunnerHelper$")
	child.Dir, child.Stdin, child.Stderr = directory, os.Stdin, os.Stderr
	child.Env = append(os.Environ(), "DELEGATE_TEAMS_PROJECT_RUNNER_HELPER=hold")
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if startErr := child.Start(); startErr != nil {
		if closeErr := output.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(startErr)
	}
	joined := false
	defer func() {
		if !joined {
			if joinErr := child.Wait(); joinErr != nil {
				t.Error(joinErr)
			}
		}
	}()
	reader := bufio.NewReader(output)
	ready, err := reader.ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("child readiness = %q, %v", ready, err)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(teamsIsolationPIDs{Parent: os.Getpid(), Child: child.Process.Pid}); encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if _, writeErr := io.WriteString(os.Stdout, ready); writeErr != nil {
		t.Fatal(writeErr)
	}
	_, copyErr := io.Copy(os.Stdout, reader)
	joinErr := child.Wait()
	joined = true
	if resultErr := errors.Join(copyErr, joinErr); resultErr != nil {
		t.Fatal(resultErr)
	}
}

type teamsIsolationPIDWriter struct {
	buffer bytes.Buffer
	ids    chan teamsIsolationPIDs
	sent   bool
}

func (writer *teamsIsolationPIDWriter) Write(data []byte) (int, error) {
	if writer.sent {
		return len(data), nil
	}
	writer.buffer.Write(data)
	newline := bytes.IndexByte(writer.buffer.Bytes(), '\n')
	if newline < 0 {
		return len(data), nil
	}
	var ids teamsIsolationPIDs
	if err := json.Unmarshal(writer.buffer.Bytes()[:newline], &ids); err != nil {
		return 0, fmt.Errorf("decode probe PID acknowledgement: %w", err)
	}
	if ids.Parent <= 1 || ids.Child <= 1 || ids.Parent == ids.Child {
		return 0, errors.New("invalid probe PID acknowledgement")
	}
	writer.sent = true
	writer.ids <- ids
	return len(data), nil
}

type teamsIsolationProcess struct {
	ready *teamsProjectReadyWriter
	ids   <-chan teamsIsolationPIDs
	input *os.File
	done  chan struct{}
	once  sync.Once
	err   error // The command's sole Wait owner writes this before closing done.
	close error
	stop  context.CancelFunc
}

func (process *teamsIsolationProcess) releaseInput() error {
	process.once.Do(func() { process.close = process.input.Close() })
	return process.close
}

func startTeamsIsolationProcess(t *testing.T, isolated, descendant bool) *teamsIsolationProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	ready := &teamsProjectReadyWriter{ready: make(chan struct{}), probe: make(chan struct{})}
	ids := &teamsIsolationPIDWriter{ids: make(chan teamsIsolationPIDs, 1)}
	process := &teamsIsolationProcess{ready: ready, ids: ids.ids, input: writer, done: make(chan struct{}), stop: cancel}
	t.Cleanup(func() {
		cancel()
		if closeErr := process.releaseInput(); closeErr != nil {
			t.Error(closeErr)
		}
		<-process.done
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if !isolated && process.err != nil {
			t.Errorf("unrelated process failed: %v", process.err)
		}
	})
	command := exec.Command(executable, "-test.run=^TestTeamsProjectRunnerHelper$")
	command.Dir, command.Stdin, command.Stdout = t.TempDir(), reader, ready
	command.Env = append(os.Environ(), "DELEGATE_TEAMS_PROJECT_RUNNER_HELPER=hold")
	if descendant {
		command = exec.Command(executable, "-test.run=^TestTeamsProjectIsolationDescendantHelper$")
		command.Dir, command.Stdin, command.Stdout = t.TempDir(), reader, io.MultiWriter(ids, ready)
		command.Env = append(os.Environ(), "DELEGATE_TEAMS_ISOLATION_DESCENDANT=1")
	}
	if isolated {
		go func() {
			process.err = (teamsProjectRunner{}).Run(ctx, command)
			close(process.done)
		}()
	} else {
		if startErr := command.Start(); startErr != nil {
			process.err = startErr
			close(process.done)
			t.Fatal(startErr)
		}
		go func() {
			process.err = command.Wait()
			close(process.done)
		}()
	}
	waitTeamsIsolationEvent(t, process, ready.ready, "readiness")
	return process
}

func waitTeamsIsolationEvent(t *testing.T, process *teamsIsolationProcess, event <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-event:
	case <-process.done:
		t.Fatalf("process ended before %s: %v", name, process.err)
	case <-time.After(15 * time.Second):
		t.Fatalf("process did not acknowledge %s", name)
	}
}

func requireTeamsIsolationResponsive(t *testing.T, process *teamsIsolationProcess) {
	t.Helper()
	if _, err := io.WriteString(process.input, "probe\n"); err != nil {
		t.Fatalf("unrelated process no longer accepts its probe: %v", err)
	}
	waitTeamsIsolationEvent(t, process, process.ready.probe, "independent probe")
}

func waitTeamsIsolationDone(t *testing.T, process *teamsIsolationProcess) error {
	t.Helper()
	select {
	case <-process.done:
		return process.err
	case <-time.After(15 * time.Second):
		t.Fatal("native command did not join its owner")
		return errors.New("native command watchdog expired")
	}
}

func TestTeamsProjectIsolationConcurrentCancellationLeavesUnrelatedProcessResponsive(t *testing.T) {
	unrelated := startTeamsIsolationProcess(t, false, false)
	firstContext, cancelFirst := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancelFirst()
	secondContext, cancelSecond := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancelSecond()
	first, firstReady := startBlockedTeamsProjectCommand(t, firstContext)
	waitTeamsProjectReady(t, first, firstReady)
	second, secondReady := startBlockedTeamsProjectCommand(t, secondContext)
	waitTeamsProjectReady(t, second, secondReady)
	cancelFirst()
	select {
	case <-first.done:
		if !errors.Is(first.err, context.Canceled) {
			t.Fatalf("first cancellation lost its cause: %v", first.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first cancelled probe was not joined")
	}
	requireTeamsIsolationResponsive(t, unrelated)
	if _, err := io.WriteString(second.input, "probe\n"); err != nil {
		t.Fatalf("concurrent probe was stopped with its sibling: %v", err)
	}
	select {
	case <-second.probe:
	case <-second.done:
		t.Fatalf("concurrent probe ended with its sibling: %v", second.err)
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent probe stopped responding")
	}
	cancelSecond()
}

func resolveIsolatedTeamsDirectory(t *testing.T) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, state, configuration := filepath.Join(base, "project"), filepath.Join(base, "state"), filepath.Join(base, "configuration")
	if mkdirErr := os.Mkdir(project, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	deps := Dependencies{Environment: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + base, "XDG_CONFIG_HOME=" + configuration, "DELEGATE_ROOT=" + state}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got, err := resolveTeamsProject(ctx, project, project, deps)
	if err != nil || got != (task.TeamProject{Kind: "directory", Path: project}) {
		t.Fatalf("in-process project resolution = %+v, %v", got, err)
	}
	for _, path := range []string{state, configuration} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("passive project resolution changed %s: %v", path, err)
		}
	}
}
