package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

type descriptorFact struct {
	FD     int    `json:"fd"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Kind   uint32 `json:"kind"`
}

func descriptor(fd int, stat unix.Stat_t) descriptorFact {
	return descriptorFact{FD: fd, Device: uint64(stat.Dev), Inode: stat.Ino, Kind: uint32(stat.Mode) & unix.S_IFMT}
}

// This actual exec runs while the resident holds E and both inherited pipe
// ends. Object identity, rather than descriptor-number absence, proves CLOEXEC:
// a Go runtime may reuse a closed descriptor number for its own poller.
func fixtureFDProbe() error {
	var execution unix.Stat_t
	if err := unix.Stat(filepath.Join(os.Getenv("DL_HOST_ROOT"), ".scheduler", "owner.lock"), &execution); err != nil {
		return err
	}
	var facts []descriptorFact
	for fd := 3; fd < 256; fd++ {
		var stat unix.Stat_t
		err := unix.Fstat(fd, &stat)
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			return err
		}
		if fd == 3 || fd == 4 || (stat.Dev == execution.Dev && stat.Ino == execution.Ino) {
			facts = append(facts, descriptor(fd, stat))
		}
	}
	if len(facts) != 3 {
		return fmt.Errorf("fixture did not identify both channel ends and E: %d", len(facts))
	}
	data, err := task.MarshalCanonical(facts)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "--host-fixture-fdprobe")
	command.Env = append(os.Environ(), "DL_HOST_FD_FACTS="+string(data))
	return command.Run()
}

func fixtureFDProbeResult() int {
	var facts []descriptorFact
	if err := task.DecodeStrict([]byte(os.Getenv("DL_HOST_FD_FACTS")), &facts); err != nil || len(facts) != 3 {
		return 100
	}
	for _, fact := range facts {
		var stat unix.Stat_t
		err := unix.Fstat(fact.FD, &stat)
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			return 101
		}
		if descriptor(fact.FD, stat) == fact {
			return 102
		}
	}
	return 0
}

// The parent owns this fixture invocation. Capture exact directory, parent and
// socket tokens while the host is live; only remove them after its exact Wait.
func pinLostHostEndpoint(t *testing.T, process *fixtureProcess, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := parent.Lstat(filepath.Base(dir))
	if err != nil {
		t.Fatal(errors.Join(err, parent.Close()))
	}
	directory, err := parent.OpenRoot(filepath.Base(dir))
	if err != nil {
		t.Fatal(errors.Join(err, parent.Close()))
	}
	socketInfo, err := directory.Lstat(filepath.Base(path))
	if err != nil {
		t.Fatal(errors.Join(err, parent.Close(), directory.Close()))
	}
	token := &endpoint{parent: parent, directory: directory, name: filepath.Base(dir), path: path, dirInfo: dirInfo, socketInfo: socketInfo}
	t.Cleanup(func() {
		if closeErr := process.release(); closeErr != nil {
			t.Error(closeErr)
		}
		receive(t, process.done)
		if _, statErr := parent.Lstat(token.name); errors.Is(statErr, os.ErrNotExist) {
			if closeErr := errors.Join(directory.Close(), parent.Close()); closeErr != nil {
				t.Error(closeErr)
			}
			return
		}
		if closeErr := token.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}
