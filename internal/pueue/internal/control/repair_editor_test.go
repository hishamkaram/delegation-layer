package control

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func repairEditorTestStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStoreWithPredicatesContext(t.Context(), filepath.Join(canonicalTemp(t), "state"), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func repairEditorSupervisorDirectory(t *testing.T, store *taskdir.Store) string {
	t.Helper()
	path := filepath.Join(store.Root, privateSupervisorDirectory)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func repairEditorTestParent(t *testing.T, path string) *controlRepairParent {
	t.Helper()
	parent := filepath.Dir(path)
	var stat unix.Stat_t
	if err := unix.Lstat(parent, &stat); err != nil {
		t.Fatal(err)
	}
	return &controlRepairParent{Path: parent, Identity: controlLeaseIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}}
}

func repairEditorTestTree(t *testing.T) (string, string) {
	t.Helper()
	return repairEditorTestTreeIn(t, canonicalTemp(t))
}

func repairEditorTestTreeIn(t *testing.T, parent string) (string, string) {
	t.Helper()
	base := filepath.Join(parent, "quote' ! café")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "7"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "7", "command")
	if err := os.WriteFile(path, []byte("old command"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, path
}

func TestRepairEditorClosesOnlyItsSharedLeaseCopy(t *testing.T) {
	store := repairEditorTestStore(t)
	lease := admissionTestLease(t, store)
	files, err := lease.DuplicateControlLeaseFiles()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeControlLeaseFiles(files); closeErr != nil {
			t.Error(closeErr)
		}
	})
	base, path := repairEditorTestTreeIn(t, repairEditorSupervisorDirectory(t, store))
	command := repairEditorTestCommand(t, store, base)
	command.ExtraFiles = files
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("managed editor failed: %v %s", runErr, output)
	}
	if content, readErr := os.ReadFile(path); readErr != nil || string(content) != "replacement" {
		t.Fatalf("editor did not replace exact command: %q %v", content, readErr)
	}
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if !errors.Is(err, task.ErrLockBusy) {
		if exclusive != nil {
			if releaseErr := exclusive.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		}
		t.Fatalf("editor unlocked caller's shared lease: %v", err)
	}
}

func TestRepairCommandNarrowsNativeUmaskEntries(t *testing.T) {
	base, path := repairEditorTestTree(t)
	if err := os.Chmod(filepath.Dir(path), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := replaceRepairCommand(base, "7", "old command", "replacement", *repairEditorTestParent(t, base)); err != nil {
		t.Fatal(err)
	}
	for selected, expectedMode := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
		info, err := os.Stat(selected)
		if err != nil || info.Mode().Perm() != expectedMode {
			t.Fatalf("native entry was not narrowed before I/O: %v %v", info, err)
		}
	}
}

func TestRepairAdmissionCloseRemovesOnlySelectedDescriptor(t *testing.T) {
	if os.Getenv("DELEGATE_TEST_REPAIR_CLOSE") == "1" {
		if err := closeRepairAdmission(os.Getenv(repairRootEnv), os.Getenv(repairRootIDEnv)); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(repairEditorFD, unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("selected admission descriptor is still open: %v", err)
		}
		if _, err := unix.FcntlInt(repairEditorFD+1, unix.F_GETFD, 0); err != nil {
			t.Fatalf("unrelated descriptor was closed: %v", err)
		}
		return
	}
	store := repairEditorTestStore(t)
	lease := admissionTestLease(t, store)
	files, err := lease.DuplicateControlLeaseFiles()
	if err != nil {
		t.Fatal(err)
	}
	decoy, err := os.CreateTemp(t.TempDir(), "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, decoy)
	t.Cleanup(func() {
		if closeErr := closeControlLeaseFiles(files); closeErr != nil {
			t.Error(closeErr)
		}
	})
	command := exec.Command(os.Args[0], "-test.run=^TestRepairAdmissionCloseRemovesOnlySelectedDescriptor$")
	command.Env = []string{"GORACE=atexit_sleep_ms=0", "DELEGATE_TEST_REPAIR_CLOSE=1", repairRootEnv + "=" + store.Root, repairRootIDEnv + "=" + store.RootID}
	command.ExtraFiles = files
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("native close-only boundary failed: %s %v", output, runErr)
	}
}

func repairEditorTestCommand(t *testing.T, store *taskdir.Store, path string) *exec.Cmd {
	command := exec.Command(os.Args[0], repairEditorMode, repairEditorToken+" "+escapePueueArgument(path))
	command.Dir = store.Root
	command.Env = []string{
		"GORACE=atexit_sleep_ms=0", repairRootEnv + "=" + store.Root, repairRootIDEnv + "=" + store.RootID,
		"PUEUE_EDIT_PATH=" + path, "DELEGATE_PUEUE_EDIT_TASK_ID=7",
		"DELEGATE_PUEUE_EDIT_EXPECTED_COMMAND=old command", "DELEGATE_PUEUE_EDIT_REPLACEMENT_COMMAND=replacement",
	}
	command.Env = repairActuatorEnvironment(command.Env, repairEditorTestParent(t, path))
	return command
}

func TestRepairEditorRefusesMismatchedAndMissingDescriptor(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(strconv.FormatBool(attached), func(t *testing.T) {
			store := repairEditorTestStore(t)
			admissionTestLease(t, store)
			base, path := repairEditorTestTreeIn(t, repairEditorSupervisorDirectory(t, store))
			command := repairEditorTestCommand(t, store, base)
			if attached {
				decoy, err := os.CreateTemp(t.TempDir(), "decoy")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if closeErr := decoy.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				})
				command.ExtraFiles = []*os.File{decoy}
			}
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("invalid descriptor edited command: %s", output)
			}
			if content, err := os.ReadFile(path); err != nil || string(content) != "old command" {
				t.Fatalf("refused editor changed command: %q %v", content, err)
			}
		})
	}
}

func TestRepairCommandRejectsMismatchAndUnsafeEntries(t *testing.T) {
	for _, scenario := range []string{"mismatch", "symlink-command", "symlink-task", "symlink-root", "hardlink", "special-command", "special-task", "public-parent"} {
		t.Run(scenario, func(t *testing.T) {
			base, path := repairEditorTestTree(t)
			applyUnsafeRepairEntry(t, scenario, base, path)
			if err := replaceRepairCommand(base, "7", "old command", "replacement", *repairEditorTestParent(t, base)); err == nil {
				t.Fatal("unsafe or changed native editor input accepted")
			}
			if scenario == "mismatch" {
				if content, err := os.ReadFile(path); err != nil || string(content) != "changed command" {
					t.Fatalf("comparison failure changed native command: %q %v", content, err)
				}
			}
		})
	}
}

func applyUnsafeRepairEntry(t *testing.T, scenario, base, path string) {
	t.Helper()
	switch scenario {
	case "mismatch":
		if err := os.WriteFile(path, []byte("changed command"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "symlink-command", "symlink-task", "symlink-root":
		selected := path
		switch scenario {
		case "symlink-task":
			selected = filepath.Dir(path)
		case "symlink-root":
			selected = base
		}
		if err := os.Rename(selected, selected+"-target"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(selected+"-target", selected); err != nil {
			t.Fatal(err)
		}
	case "hardlink":
		if err := os.Link(path, path+"-other"); err != nil {
			t.Fatal(err)
		}
	case "special-command", "special-task", "public-parent":
		selected, mode := path, os.FileMode(0o600)|os.ModeSetuid
		switch scenario {
		case "special-task":
			selected, mode = filepath.Dir(path), 0o700|os.ModeSticky
		case "public-parent":
			selected, mode = filepath.Dir(base), 0o755
		}
		if err := os.Chmod(selected, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepairRoleSelectsSingleDescriptorIndependentOfLeaseIndex(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	store := repairEditorTestStore(t)
	lease := admissionTestLease(t, store)
	decoy, err := os.CreateTemp(t.TempDir(), "decoy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := decoy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	ctx := WithControlLease(WithControlLease(t.Context(), qualifiedFixtureLease{files: []*os.File{decoy}}), lease)
	repairEditorSupervisorDirectory(t, store)
	parent, err := canonicalRepairParent(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	fake.client.runtimeBinding.ConfigPath = filepath.Join(parent.Path, "repair.yml")
	scoped, err := fake.client.admissionCommandContext(ctx, controlAdmissionRepair, store.RootID, store.Root)
	if err != nil {
		t.Fatal(err)
	}
	admissionScopeFrom(scoped).repairParent = &parent
	duplicates, err := duplicateControlLeaseFiles(scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeControlLeaseFiles(duplicates.files); closeErr != nil {
			t.Error(closeErr)
		}
	})
	request := controlOwnerRequest{Admission: duplicates.admission, Arguments: []string{fake.executable, "-c", fake.client.runtimeBinding.ConfigPath, "edit", "7"}}
	if err = validateControlOwnerForwarding(request); err != nil {
		t.Fatal(err)
	}
	files, err := controlOwnerForwardedFiles(request, duplicates.files)
	if err != nil || len(files) != 1 || duplicates.admission.LeaseIndex != 1 || files[0] != duplicates.files[1] {
		t.Fatalf("repair CLI did not select exactly fd3 from actual lease: %+v %v", duplicates.admission, err)
	}
	if _, err = unix.FcntlInt(files[0].Fd(), unix.F_GETFD, 0); err != nil {
		t.Fatal(err)
	}
	request.Arguments[4] = "07"
	if err = validateControlOwnerForwarding(request); err == nil {
		t.Fatal("noncanonical repair task id accepted")
	}
}

func TestManagedRepairRefusesMissingResourceAndFallback(t *testing.T) {
	fixture := newRunnerUpgradeProbeFixture(t)
	client := fixture.supervisor.client
	client.options.ControlOwnerExecutable = os.Args[0]
	if err := client.replaceQueuedRunnerCommand(t.Context(), 7, "old", "new"); !errors.Is(err, ErrRootAdmissionRequired) {
		t.Fatalf("managed repair without actual lease: %v", err)
	}
	store := repairEditorTestStore(t)
	repairEditorSupervisorDirectory(t, store)
	ctx := WithControlLease(t.Context(), admissionTestLease(t, store))
	if err := client.replaceQueuedRunnerCommand(ctx, 7, "old", "new"); !errors.Is(err, ErrRootAdmissionRequired) {
		t.Fatalf("another root's actual lease authorized repair: %v", err)
	}
	client.options.ControlOwnerExecutable = ""
	if err := client.replaceQueuedRunnerCommand(ctx, 7, "old", "new"); !errors.Is(err, ErrRootAdmissionRequired) {
		t.Fatalf("attached lease fell back to shell repair: %v", err)
	}
	if commands := readInvocations(t, fixture.supervisor.logPath); countCommand(commands, "edit") != 0 {
		t.Fatalf("refused repair launched a CLI: %v", commands)
	}
}

func TestRepairConfigPublishesBoundManagedImage(t *testing.T) {
	base := canonicalTemp(t)
	source := []byte(privateConfigYAML(base))
	path := filepath.Join(base, "source.yml")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	editor, err := canonicalExecutable(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	derived, err := privateRepairConfig(base, path, task.ComputeSHA256(source), editor)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(derived), "pueue-repair-") {
		t.Fatal("repair reused legacy derived config")
	}
	if _, err = privateRepairConfig(base, path, strings.Repeat("0", 64), editor); !errors.Is(err, ErrBinding) {
		t.Fatalf("changed source accepted: %v", err)
	}
}

func TestRepairParentRejectsReplacedNativeDirectory(t *testing.T) {
	store := repairEditorTestStore(t)
	path := repairEditorSupervisorDirectory(t, store)
	parent, err := canonicalRepairParent(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = verifyRepairParent(&parent); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("same-path replacement acquired repair authority: %v", err)
	}
}

func TestRepairActuatorRequiresOneDescriptorAndOverridesHostileTemp(t *testing.T) {
	store := repairEditorTestStore(t)
	repairEditorSupervisorDirectory(t, store)
	parent, err := canonicalRepairParent(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	admission := &controlOwnerAdmission{Role: controlAdmissionRepair, RepairParent: &parent}
	for _, count := range []int{0, 2} {
		if _, err = controlOwnerEnvironment(admission, count); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("repair actuator allowed %d descriptors: %v", count, err)
		}
	}
	original := []string{"TMPDIR=/unavailable/hostile-temp", repairParentEnv + "=unsafe"}
	derived := repairActuatorEnvironment(original, &parent)
	if original[0] != "TMPDIR=/unavailable/hostile-temp" || environmentValue(derived, "TMPDIR") != parent.Path || environmentValue(derived, repairParentEnv) != parent.Path {
		t.Fatal("repair environment did not derive the private native parent immutably")
	}
}
