package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSupervisorStopOwnerArgumentBoundary(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "stop-owner", "--root", "/state", "--json"},
		{"--root", "/state", "supervisor", "stop-owner"},
	} {
		parsed, err := ParseArguments(args)
		if err != nil || parsed.Command != "supervisor" || parsed.SupervisorAction != "stop-owner" || parsed.Root != "/state" {
			t.Fatalf("existing-owner stop not exposed through supervisor grammar: %+v %v", parsed, err)
		}
	}
	for _, args := range [][]string{
		{"supervisor", "stop-owner", "extra"},
		{"supervisor", "stop-owner", "--runner", "/installed/runner"},
		{"supervisor", "stop-owner", "--pueue-config", "/state/pueue.yml"},
	} {
		if parsed, err := ParseArguments(args); err == nil {
			t.Fatalf("owner stop accepted a foreign selection: %+v", parsed)
		}
	}
}

func TestSupervisorStopOwnerReadsExistingStateWithoutInstalledRunner(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[initialized], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if initialized {
				store, err := taskdir.InitStore(root)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			assertOwnerStopWithoutInstallation(t, root)
			assertOwnerStopDidNotInitialize(t, root, initialized)
		})
	}
}

func assertOwnerStopDidNotInitialize(t *testing.T, root string, initialized bool) {
	t.Helper()
	if !initialized {
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("passive stop initialized absent root: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "infrastructure")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive stop initialized infrastructure: %v", err)
	}
}

func assertOwnerStopWithoutInstallation(t *testing.T, root string) {
	t.Helper()
	missingRunner := filepath.Join(t.TempDir(), "missing-installed-runner")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"supervisor", "stop-owner", "--root", root, "--json"}, &stdout, &stderr, Dependencies{
		RunnerExecutable: missingRunner, InitialSupervisorExecutable: missingRunner,
		Environment: []string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()},
	})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	owner := response.SupervisorOwner
	if code != 0 || owner == nil || owner.Action != "stop-owner" || owner.State != infrastructure.OwnerNotInitialized || owner.ProcessClosure != infrastructure.OwnerProcessUnknown || owner.Startup != (infrastructure.StartupReference{}) {
		t.Fatalf("passive owner stop required an installation: code=%d response=%+v stderr=%q", code, response, stderr.String())
	}
	if response.Supervisor != nil || response.SupervisorControl != nil || response.Stop != nil || len(response.Stops) != 0 {
		t.Fatalf("owner stop claimed Pueue/task control: %+v", response)
	}
}

func TestSupervisorStopOwnerHumanOutputKeepsClosureUnknown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"supervisor", "stop-owner", "--root", filepath.Join(t.TempDir(), "absent")}, &stdout, &stderr, Dependencies{})
	if code != 0 || !strings.Contains(stdout.String(), "supervisor_owner action=stop-owner state=not_initialized process_closure=unknown") {
		t.Fatalf("human owner stop lost serving/process distinction: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSupervisorStopOwnerDoesNotTurnUnreadableStartupIntoAbsence(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	root := store.Root
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "infrastructure")
	if err = os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// Canonical sequence filename, deliberately invalid record DATA. A failed
	// read cannot authorize initialization, migration, a replacement or absence.
	record := filepath.Join(directory, "intent-00000000000000000001.json")
	before := []byte("{\"sequence\":1}\n")
	if err = os.WriteFile(record, before, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"supervisor", "stop-owner", "--root", root, "--json"}, &stdout, &stderr, Dependencies{})
	var response Response
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if code != 1 || response.SupervisorOwner == nil || response.SupervisorOwner.State != infrastructure.OwnerStopUnknown || response.Error == "" {
		t.Fatalf("unreadable owner became a warm absent result: code=%d response=%+v stderr=%q", code, response, stderr.String())
	}
	after, err := os.ReadFile(record)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("passive stop repaired its unreadable input: %q %v", after, err)
	}
	if _, err = os.Stat(filepath.Join(root, ".infrastructure-owner.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive stop created an election lease: %v", err)
	}
}
