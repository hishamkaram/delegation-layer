package harnesscli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestEventRecorderWritesImmutableWrapperAndSupervisorReceipts(t *testing.T) {
	dir := t.TempDir()
	recorder := newEventRecorder(HarnessConfig{EventsDirectory: dir, Hooks: HookConfig{Mode: HookNormal}})
	argv := []string{"delegate", "dispatch", "--provider", "fixture:test", "$(literal)"}
	if err := recorder.begin(argv); err != nil {
		t.Fatal(err)
	}
	commandID := "11111111111111111111111111111111"
	commandArgv := []string{"pueue", "-c", "/private/pueue.yml", "add", "literal;token"}
	recorder.supervisorEvent(pueue.CommandEvent{CommandID: commandID, Stage: "entry", Argv: commandArgv, ExitCode: -1})
	recorder.supervisorEvent(pueue.CommandEvent{CommandID: commandID, Stage: "started", Argv: commandArgv, PID: 42, ExitCode: -1})
	recorder.supervisorEvent(pueue.CommandEvent{CommandID: commandID, Stage: "completed", Argv: commandArgv, PID: 42, ExitCode: 0})
	recorder.executionEvent("start-entry")
	recorder.executionEvent("completion-observed")
	if err := recorder.finish(0, ""); err != nil {
		t.Fatal(err)
	}
	if err := recorder.finish(0, ""); !errors.Is(err, errRecorderDuplicate) {
		t.Fatalf("second wrapper completion error=%v", err)
	}

	recorder.mu.Lock()
	wrapperID := recorder.wrapper.receipt.InvocationID
	recorder.mu.Unlock()
	assertWrapperReceipts(t, dir, wrapperID, argv)
	assertSupervisorReceipts(t, dir, commandID, commandArgv)
}

func assertWrapperReceipts(t *testing.T, dir, wrapperID string, argv []string) {
	t.Helper()
	var entry processReceipt
	readCanonical(t, filepath.Join(dir, wrapperID, "entry.json"), &entry)
	if entry.InvocationID != wrapperID || !equalStringSlices(entry.Argv, argv) || entry.ExitCode != -1 {
		t.Fatalf("wrapper entry=%+v", entry)
	}
	var completion processReceipt
	readCanonical(t, filepath.Join(dir, wrapperID, "completion.json"), &completion)
	if completion.InvocationID != wrapperID || completion.ExitCode != 0 || completion.Failed {
		t.Fatalf("wrapper completion=%+v", completion)
	}
	var event eventEnvelope
	readCanonical(t, filepath.Join(dir, wrapperID, "event-000001.json"), &event)
	if event.Kind != "execution" || event.Name != "start-entry" || event.Sequence != 1 {
		t.Fatalf("wrapper event=%+v", event)
	}
}

func assertSupervisorReceipts(t *testing.T, dir, commandID string, commandArgv []string) {
	t.Helper()
	var supervisor supervisorCompletion
	readCanonical(t, filepath.Join(dir, commandID, "completion.json"), &supervisor)
	if supervisor.CommandID != commandID || supervisor.PID != 42 || supervisor.ExitCode != 0 || supervisor.Failed {
		t.Fatalf("supervisor completion=%+v", supervisor)
	}
	var supervisorEntry eventEnvelope
	readCanonical(t, filepath.Join(dir, commandID, "entry.json"), &supervisorEntry)
	if supervisorEntry.Kind != "supervisor" || supervisorEntry.Command == nil || !equalStringSlices(supervisorEntry.Command.Argv, commandArgv) {
		t.Fatalf("supervisor entry=%+v", supervisorEntry)
	}
}

func TestEventRecorderSerializesConcurrentSupervisorCommands(t *testing.T) {
	dir := t.TempDir()
	recorder := newEventRecorder(HarnessConfig{EventsDirectory: dir, Hooks: HookConfig{Mode: HookNormal}})
	if err := recorder.begin([]string{"delegate-run", "--root", "/private/root", "task"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("%032x", i+1)
			argv := []string{"pueue", "-c", "/private/pueue.yml", "status", "--json"}
			recorder.supervisorEvent(pueue.CommandEvent{CommandID: id, Stage: "entry", Argv: argv, ExitCode: -1})
			recorder.supervisorEvent(pueue.CommandEvent{CommandID: id, Stage: "started", Argv: argv, PID: i + 1, ExitCode: -1})
			recorder.supervisorEvent(pueue.CommandEvent{CommandID: id, Stage: "completed", Argv: argv, PID: i + 1, ExitCode: 0})
		}()
	}
	wg.Wait()
	if err := recorder.finish(0, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 17 {
		t.Fatalf("event directories=%d want 17", len(entries))
	}
}

func TestPublicationDelayedEventAllowsConcurrentSupervisorCompletion(t *testing.T) {
	dir := t.TempDir()
	cfg := HarnessConfig{EventsDirectory: dir, Hooks: HookConfig{Mode: HookPublicationDelayed}}
	recorder := newEventRecorder(cfg)
	if err := recorder.begin([]string{"delegate-run", "--root", "/private/root", "task"}); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	wrapperID := recorder.wrapper.receipt.InvocationID
	recorder.mu.Unlock()
	publicationEntered := make(chan struct{})
	publicationRelease := make(chan struct{})
	publishedDone := make(chan struct{})
	release := sync.OnceFunc(func() { close(publicationRelease) })
	hooks := recorder.executionHooksWithWait(cfg.Hooks, func(HookConfig) error {
		close(publicationEntered)
		<-publicationRelease
		return nil
	})
	go func() {
		hooks.Event("published")
		close(publishedDone)
	}()
	t.Cleanup(func() {
		release()
		<-publishedDone
	})
	select {
	case <-publicationEntered:
	case <-publishedDone:
		t.Fatal("publication-delayed hook returned before wait callback entered")
	}

	publishedPath := filepath.Join(dir, wrapperID, "event-000001.json")
	var event eventEnvelope
	readCanonical(t, publishedPath, &event)
	if event.Name != "published" || event.Sequence != 1 {
		t.Fatalf("publication event=%+v", event)
	}
	if !recorder.mu.TryLock() {
		release()
		t.Fatal("publication wait retained the recorder mutex")
	}
	recorder.mu.Unlock()

	commandID := strings.Repeat("1", 32)
	commandArgv := []string{"pueue", "-c", "/private/pueue.yml", "kill", "7"}
	recorder.supervisorEvent(pueue.CommandEvent{CommandID: commandID, Stage: "entry", Argv: commandArgv, ExitCode: -1})
	recorder.supervisorEvent(pueue.CommandEvent{CommandID: commandID, Stage: "started", Argv: commandArgv, PID: 42, ExitCode: -1})
	recorder.supervisorEvent(pueue.CommandEvent{CommandID: commandID, Stage: "completed", Argv: commandArgv, PID: 42, ExitCode: 0})
	assertSupervisorReceipts(t, dir, commandID, commandArgv)
	select {
	case <-publishedDone:
		t.Fatal("publication-delayed event returned before explicit release")
	default:
	}
	release()
	<-publishedDone
	if err := recorder.finish(0, ""); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationDelayedHookLatchesWaitError(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "release-parent")
	if err := os.WriteFile(parent, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := HarnessConfig{EventsDirectory: dir, Hooks: HookConfig{Mode: HookPublicationDelayed, ReleasePath: filepath.Join(parent, "publication.release")}}
	recorder := newEventRecorder(cfg)
	if err := recorder.begin([]string{"delegate-run", "--root", "/private/root", "task"}); err != nil {
		t.Fatal(err)
	}
	recorder.executionHooks(cfg.Hooks).Event("published")
	if err := recorder.finish(0, ""); err == nil {
		t.Fatal("publication wait error was not latched")
	}
	recorder.mu.Lock()
	wrapperID := recorder.wrapper.receipt.InvocationID
	recorder.mu.Unlock()
	var completion processReceipt
	readCanonical(t, filepath.Join(dir, wrapperID, "completion.json"), &completion)
	if !completion.Failed || completion.Error == "" {
		t.Fatalf("latched publication wait error missing from completion=%+v", completion)
	}
}

func TestExecutionHooksUseFiniteRealOperations(t *testing.T) {
	cmd := exec.Command("/bin/echo", "literal;token")
	startErr := startFailed(cmd)
	if startErr == nil {
		t.Fatal("start-failed hook unexpectedly started a process")
	}

	var captured bytes.Buffer
	captureErr := captureFailed("stdout", &captured, strings.NewReader(strings.Repeat("x", hookCapturePrefix+10)))
	if !errors.Is(captureErr, errCaptureHook) || captured.Len() != hookCapturePrefix {
		t.Fatalf("capture hook err=%v bytes=%d", captureErr, captured.Len())
	}

	start := time.Now()
	if err := waitForHook(HookConfig{Mode: HookStartDelayed, DelayMS: 15}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond || elapsed > time.Second {
		t.Fatalf("start delay=%s", elapsed)
	}
	release := filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := waitForHook(HookConfig{Mode: HookStartDelayed, DelayMS: 10_000, ReleasePath: release}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("existing release did not unblock delayed start")
	}

	cmd = exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitFailed(cmd); !errors.Is(err, errWaitHook) {
		t.Fatal(err)
	}
}

func readCanonical(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = task.DecodeStrict(data, value); err != nil {
		t.Fatal(err)
	}
}
