package guardian

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == LaunchGateMode {
		if err := RunLaunchGate(os.Args[2:], unix.Exec); err != nil {
			os.Exit(31)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type projectLaunchProbe struct {
	Device           uint64 `json:"device"`
	Inode            uint64 `json:"inode"`
	InheritedProject bool   `json:"inherited_project"`
}

func TestProjectLaunchProvider(t *testing.T) {
	marker := os.Getenv("DELEGATE_PROJECT_LAUNCH_MARKER")
	if marker == "" {
		return
	}
	var current unix.Stat_t
	if err := unix.Stat(".", &current); err != nil {
		os.Exit(32)
	}
	probe := projectLaunchProbe{Device: uint64(current.Dev), Inode: uint64(current.Ino)}
	for fd := 3; fd < 32; fd++ {
		var inherited unix.Stat_t
		err := unix.Fstat(fd, &inherited)
		if err != nil && !errors.Is(err, unix.EBADF) {
			os.Exit(33)
		}
		if err == nil && inherited.Mode&unix.S_IFMT == unix.S_IFDIR && inherited.Dev == current.Dev && inherited.Ino == current.Ino {
			probe.InheritedProject = true
		}
	}
	data, err := json.Marshal(probe)
	if err != nil {
		os.Exit(34)
	}
	if err = os.WriteFile(marker, data, 0o600); err != nil {
		os.Exit(35)
	}
	os.Exit(0)
}

func projectLaunchDirectory(t *testing.T) *ProjectDirectory {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "project")
	if err = os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLifetimeLaunchGateTestFile(t, file) })
	var identity unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &identity); err != nil {
		t.Fatal(err)
	}
	return &ProjectDirectory{File: file, Identity: task.DirectoryIdentity{Path: path, Device: uint64(identity.Dev), Inode: uint64(identity.Ino)}}
}

func TestProjectLaunchGatePinsDirectoryAndClosesChildCustody(t *testing.T) {
	assertProjectLaunch(t, false)
}

func TestProjectLaunchGateRefusesReplacementAfterAcknowledgment(t *testing.T) {
	assertProjectLaunch(t, true)
}

func TestProjectLaunchFactoryRejectsWrongBindingWithoutClosingBorrow(t *testing.T) {
	project := projectLaunchDirectory(t)
	project.Identity.Inode++
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable)
	if _, err = NewLaunchGateFactory(executable)(command, project); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("wrong project binding was accepted: %v", err)
	}
	if _, err = project.File.Stat(); err != nil || len(command.ExtraFiles) != 0 {
		t.Fatalf("refused factory changed borrowed custody: stat=%v inherited=%d", err, len(command.ExtraFiles))
	}
}

type projectLaunchProcess struct {
	command         *exec.Cmd
	gate            LaunchGate
	started, waited bool
}

func (p *projectLaunchProcess) close(t *testing.T) {
	t.Helper()
	if err := p.gate.Abort(); err != nil {
		t.Error(err)
	}
	if p.started && !p.waited {
		p.waited = true
		if err := p.command.Wait(); err != nil && !t.Failed() {
			t.Error(err)
		}
	}
}

func startProjectLaunchProcess(t *testing.T, project *ProjectDirectory, marker string) *projectLaunchProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestProjectLaunchProvider$")
	command.Env = append(os.Environ(), "DELEGATE_PROJECT_LAUNCH_MARKER="+marker)
	// A distinct bootstrap cwd demonstrates that the retained FD performs the
	// final child chdir; merely retaining cmd.Dir would fail the success oracle.
	command.Dir = t.TempDir()
	gate, err := NewLaunchGateFactory(executable)(command, project)
	if err != nil {
		t.Fatal(err)
	}
	process := &projectLaunchProcess{command: command, gate: gate}
	t.Cleanup(func() { process.close(t) })
	if err = command.Start(); err != nil {
		t.Fatal(errors.Join(err, gate.Started(err)))
	}
	process.started = true
	if err = gate.Started(nil); err != nil {
		t.Fatal(err)
	}
	return process
}

func assertProjectLaunch(t *testing.T, replace bool) {
	t.Helper()
	project := projectLaunchDirectory(t)
	parentCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "provider.json")
	process := startProjectLaunchProcess(t, project, marker)
	if replace {
		if err = os.Rename(project.Identity.Path, project.Identity.Path+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(project.Identity.Path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err = process.gate.Release(); err != nil {
		t.Fatal(err)
	}
	process.waited = true
	waitErr := process.command.Wait()
	if _, err = project.File.Stat(); err != nil {
		t.Fatal("factory closed borrowed parent custody:", err)
	}
	gotCwd, err := os.Getwd()
	if err != nil || gotCwd != parentCwd {
		t.Fatalf("child handoff changed parent cwd: %q, %v", gotCwd, err)
	}
	assertProjectLaunchProbe(t, project.Identity, marker, replace, waitErr)
}

func assertProjectLaunchProbe(t *testing.T, identity task.DirectoryIdentity, marker string, refused bool, waitErr error) {
	t.Helper()
	data, err := os.ReadFile(marker)
	if refused {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) || exit.ExitCode() != 31 || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("replacement reached provider: Wait=%v marker=%v", waitErr, err)
		}
		return
	}
	if waitErr != nil || err != nil {
		t.Fatal(errors.Join(waitErr, err))
	}
	var probe projectLaunchProbe
	if err = json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Device != identity.Device || probe.Inode != identity.Inode || probe.InheritedProject {
		t.Fatalf("provider did not receive isolated initial cwd: %+v, expected %+v", probe, identity)
	}
}
