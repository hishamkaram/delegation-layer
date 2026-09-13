package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var fixtureBinPath string

func TestMain(m *testing.M) {
	temp, err := os.MkdirTemp("", "protocolfixture-suite-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fixtureBinPath = filepath.Join(temp, "protocolfixture")
	command := exec.Command("go", "build", "-buildvcs=false", "-o", fixtureBinPath, ".")
	code := 1
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "fixture build: %v\n%s", buildErr, output)
	} else {
		code = m.Run()
	}
	if removeErr := os.RemoveAll(temp); removeErr != nil {
		fmt.Fprintf(os.Stderr, "fixture binary cleanup: %v\n", removeErr)
		code = 1
	}
	os.Exit(code)
}

type (
	fixtureCase   struct{ root, id, brief, cwd, sink, base string }
	fixtureResult struct {
		stdout, stderr string
		code           int
	}
)

func newCase(t *testing.T) *fixtureCase {
	t.Helper()
	base := t.TempDir()
	id, err := task.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	c := &fixtureCase{base: base, root: filepath.Join(base, "store"), id: id, brief: filepath.Join(base, "brief.md"), cwd: filepath.Join(base, "workspace"), sink: filepath.Join(base, "sink.log")}
	if err := os.Mkdir(c.cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, c.brief, []byte("test brief\n"))
	writeTestFile(t, c.sink, nil)
	t.Cleanup(func() { c.logInventory(t) })
	return c
}

func (c *fixtureCase) logInventory(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(c.root); errors.Is(err, os.ErrNotExist) {
		t.Logf("store inventory: root absent %q", c.root)
		return
	}
	if err := filepath.WalkDir(c.root, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		snapshot(t, path)
		return nil
	}); err != nil {
		t.Errorf("recording store inventory: %v", err)
	}
	snapshot(t, c.sink)
}

func (c *fixtureCase) args(command string, args ...string) []string {
	prefix := []string{command, "-root", c.root}
	if command != "scavenge" {
		prefix = append(prefix, "-task-id", c.id)
	}
	return append(prefix, args...)
}

func executeTestFixture(t *testing.T, args ...string) fixtureResult {
	t.Helper()
	cmd := exec.Command(fixtureBinPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := fixtureResult{stdout: stdout.String(), stderr: stderr.String(), code: processExit(t, err)}
	logFixtureResult(t, cmd, result)
	return result
}

func logFixtureResult(t *testing.T, cmd *exec.Cmd, result fixtureResult) {
	t.Helper()
	controlOutput := result.stdout
	if len(controlOutput) > 32*1024 {
		controlOutput = "large output omitted; exact length and digest retained"
	}
	t.Logf("fixture pid=%d args=%q exit=%d stdout_bytes=%d stdout_sha256=%s stdout=%q stderr=%q", cmd.Process.Pid, cmd.Args, result.code, len(result.stdout), task.ComputeSHA256([]byte(result.stdout)), controlOutput, result.stderr)
}

func processExit(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatalf("starting/waiting for finite fixture: %v", err)
	}
	return exited.ExitCode()
}

func (c *fixtureCase) run(t *testing.T, want int, command string, args ...string) fixtureResult {
	t.Helper()
	result := executeTestFixture(t, c.args(command, args...)...)
	if result.code != want {
		t.Fatalf("%s exit=%d want=%d stdout=%s stderr=%s", command, result.code, want, result.stdout, result.stderr)
	}
	return result
}

func (c *fixtureCase) prepare(t *testing.T, extra ...string) {
	t.Helper()
	c.run(t, 0, "prepare", append([]string{"-canonical-cwd", c.cwd, "-brief-file", c.brief}, extra...)...)
}

func (c *fixtureCase) admit(t *testing.T) {
	t.Helper()
	c.run(t, 0, "claim", "-kind", "admission", "-fake-sink-event", c.sink)
}

func (c *fixtureCase) seal(t *testing.T, content string, extra ...string) {
	t.Helper()
	c.run(t, 0, "seal", append([]string{"-raw-stdout", content, "-fake-sink-event", c.sink}, extra...)...)
}

func preparedCase(t *testing.T) *fixtureCase {
	t.Helper()
	c := newCase(t)
	c.prepare(t)
	c.admit(t)
	return c
}

func sealedCase(t *testing.T, content string) *fixtureCase {
	t.Helper()
	c := preparedCase(t)
	c.seal(t, content)
	return c
}
func (c *fixtureCase) path(name string) string { return filepath.Join(c.root, "tasks", c.id, name) }

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", path, err)
	}
}

type fileSnapshot struct {
	data []byte
	info os.FileInfo
}

func snapshot(t *testing.T, path string) fileSnapshot {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	switch {
	case info.Mode().IsRegular():
		data = readTestFile(t, path)
	case info.Mode()&os.ModeSymlink != 0:
		target, readErr := os.Readlink(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		data = []byte(target)
	}
	t.Logf("snapshot path=%q mode=%s bytes=%d sha256=%s identity=%+v", path, info.Mode(), len(data), task.ComputeSHA256(data), info.Sys())
	return fileSnapshot{data: data, info: info}
}

func assertSnapshot(t *testing.T, path string, before fileSnapshot) {
	t.Helper()
	after := snapshot(t, path)
	if !os.SameFile(before.info, after.info) || before.info.Mode() != after.info.Mode() || !bytes.Equal(before.data, after.data) {
		t.Fatalf("immutable file changed: %s", path)
	}
}

func (c *fixtureCase) sinkCount(t *testing.T, event string) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(readTestFile(t, c.sink))), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 || parts[2] != c.id {
			t.Fatalf("malformed sink entry: %q", line)
		}
		if _, err := strconv.Atoi(parts[0]); err != nil {
			t.Fatal(err)
		}
		if parts[1] == event {
			count++
		}
	}
	return count
}

func (c *fixtureCase) assertSink(t *testing.T, event string, want int) {
	t.Helper()
	if n := c.sinkCount(t, event); n != want {
		t.Fatalf("%s count=%d want=%d", event, n, want)
	}
}

func (c *fixtureCase) inspect(t *testing.T, want task.Publication) taskdir.TaskInspection {
	t.Helper()
	result := c.run(t, 0, "inspect")
	var observation taskdir.TaskInspection
	if err := json.Unmarshal([]byte(result.stdout), &observation); err != nil {
		t.Fatal(err)
	}
	if observation.Publication != want {
		t.Fatalf("publication=%s want=%s: %s", observation.Publication, want, result.stdout)
	}
	return observation
}

func (c *fixtureCase) collectExact(t *testing.T, content string, verdict string) {
	t.Helper()
	before := snapshot(t, c.sink)
	result := c.run(t, 0, "collect", "-raw-output")
	if result.stdout != content {
		t.Fatalf("raw result differs: got=%q want=%q", result.stdout, content)
	}
	var record task.OutcomeRecord
	if err := task.DecodeStrict(readTestFile(t, c.path("outcome.json")), &record); err != nil {
		t.Fatal(err)
	}
	if record.Verdict != verdict || record.Payload.Length != int64(len(content)) || record.Payload.SHA256 != task.ComputeSHA256([]byte(content)) {
		t.Fatalf("incorrect terminal descriptor: %+v", record)
	}
	assertSnapshot(t, c.sink, before)
}

func assertCheckpoint(t *testing.T, path, point string) int {
	t.Helper()
	return awaitCheckpoint(t, path, point, 0)
}

func awaitCheckpoint(t *testing.T, path, point string, expectedPID int) int {
	t.Helper()
	pid, err := waitForRendezvous(path, point, expectedPID, 12*time.Second)
	if err != nil {
		t.Fatalf("checkpoint %q not observed at %q: %v", point, path, err)
	}
	t.Logf("checkpoint pid=%d point=%q path=%q", pid, point, path)
	return pid
}

func (c *fixtureCase) crash(t *testing.T, point, command string, args ...string) string {
	t.Helper()
	rv := filepath.Join(t.TempDir(), "checkpoint")
	extra := append(args, "-checkpoint", point, "-rendezvous", rv)
	c.run(t, 3, command, extra...)
	assertCheckpoint(t, rv, point)
	return rv
}

func assertEventOrder(t *testing.T, path string, expected ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(readTestFile(t, path))), "\n")
	t.Logf("ordered trace path=%q events=%q", path, lines)
	next := 0
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			t.Fatalf("malformed event: %q", line)
		}
		if next < len(expected) && parts[1]+":"+parts[2] == expected[next] {
			next++
		}
	}
	if next != len(expected) {
		t.Fatalf("missing/out-of-order event %q in %v", expected[next], lines)
	}
}

type fixtureProcess struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	done           chan struct{}
	err            error
	want           int
	finished       bool
	release        string
}

func startFixture(t *testing.T, want int, release string, args ...string) *fixtureProcess {
	t.Helper()
	p := &fixtureProcess{cmd: exec.Command(fixtureBinPath, args...), done: make(chan struct{}), want: want, release: release}
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		if !p.finished {
			if p.release != "" {
				writeTestFile(t, p.release, []byte("release\n"))
			}
			p.wait(t)
		}
	})
	return p
}

func (p *fixtureProcess) wait(t *testing.T) fixtureResult {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("finite fixture PID%d did not exit; args=%v", p.cmd.Process.Pid, p.cmd.Args)
	}
	p.finished = true
	result := fixtureResult{stdout: p.stdout.String(), stderr: p.stderr.String(), code: processExit(t, p.err)}
	logFixtureResult(t, p.cmd, result)
	if result.code != p.want {
		t.Fatalf("finite fixture exit=%d want=%d stdout=%s stderr=%s", result.code, p.want, result.stdout, result.stderr)
	}
	return result
}

func (p *fixtureProcess) releaseAndWait(t *testing.T) fixtureResult {
	t.Helper()
	writeTestFile(t, p.release, []byte("release\n"))
	return p.wait(t)
}

func (c *fixtureCase) hold(t *testing.T, point, command string, args ...string) (*fixtureProcess, string) {
	t.Helper()
	base := t.TempDir()
	rv := filepath.Join(base, "ready")
	release := filepath.Join(base, "release")
	args = append(args, "-checkpoint", point, "-rendezvous", rv, "-release-file", release)
	p := startFixture(t, 0, release, c.args(command, args...)...)
	awaitCheckpoint(t, rv, point, p.cmd.Process.Pid)
	return p, rv
}

func capturePresent(t *testing.T, paths ...string) map[string]fileSnapshot {
	t.Helper()
	saved := make(map[string]fileSnapshot)
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		saved[path] = snapshot(t, path)
	}
	return saved
}
