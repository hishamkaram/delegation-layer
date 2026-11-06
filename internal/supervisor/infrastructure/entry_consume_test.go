package infrastructure

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// This validator deliberately accepts the Go test image to exercise component
// premises only. It is not managed-image eligibility or product authority proof.
type componentEntryHost struct {
	reject bool
	calls  int
}

func (h *componentEntryHost) ValidateRunner(ctx context.Context, root, runner string, info *debug.BuildInfo) error {
	h.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.reject || root == "" || runner == "" || info == nil {
		return ErrIdentity
	}
	flags, err := unix.FcntlInt(3, unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return errors.New("entry callback observed inheritable instance descriptor")
	}
	return nil
}

func (*componentEntryHost) EnsureOwnRoot(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
	return Disposition{}, errors.New("component must not prepare supervisor")
}

func (*componentEntryHost) RecoverSaved(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
	return Disposition{}, errors.New("component must not recover supervisor")
}

func (*componentEntryHost) PrepareContinuation(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
	return Disposition{}, errors.New("component must not prepare continuation")
}

func (*componentEntryHost) MigrateOwnRoot(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, ManualMigrationReport, error) {
	return Disposition{}, ManualMigrationReport{}, errors.New("component must not migrate own supervisor")
}

func (*componentEntryHost) ClassifyPreparation(err error) PreparationFailure {
	if err == nil {
		return PreparationFailure{}
	}
	failure := PreparationFailure{}
	if retainsResidentCleanup(err) {
		failure.IndependentFault = true
	}
	task.AnyErrorNode(err, func(node error) bool {
		if _, joined := node.(interface{ Unwrap() []error }); joined || errors.Unwrap(node) != nil {
			return false
		}
		cause, known := componentPreparationCause(node)
		failure.Causes |= cause
		if !known {
			failure.IndependentFault = true
		}
		return false
	})
	return failure
}

func componentPreparationCause(err error) (PreparationCauses, bool) {
	value := reflect.ValueOf(err)
	if !value.IsValid() || !value.Comparable() {
		return 0, false
	}
	for _, mapping := range []struct {
		err   error
		cause PreparationCauses
	}{
		{task.ErrLockBusy, PreparationCauseLockBusy},
		{ErrOwnerBusy, PreparationCauseLockBusy},
		{context.DeadlineExceeded, PreparationCauseDeadline},
		{context.Canceled, PreparationCauseCanceled},
		{ErrOwnerIncompatible, 0},
		{ErrOwnerIncompatible, 0},
	} {
		// Equal compares exact pointer/value identity without invoking a leaf's Is method.
		if value.Equal(reflect.ValueOf(mapping.err)) {
			return mapping.cause, true
		}
	}
	return 0, false
}

type componentEntryFixture struct {
	root      string
	store     *taskdir.Store
	journal   *intentJournal
	lease     *Lease
	intent    StartupIntent
	reference StartupReference
	retain    bool
}

func newComponentEntryFixture(t *testing.T, kind string) *componentEntryFixture {
	t.Helper()
	switch kind {
	case "task-contained", "foreign-descriptor", "wrong-descriptor", "bare-descriptor":
		// These negative cases refuse at/before placement and need no outside host.
	default:
		requireComponentEntryTestHost(t)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".ie-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &componentEntryFixture{root: root}
	t.Cleanup(func() {
		if closeErr := fixture.Close(); closeErr != nil {
			t.Errorf("retain component fixture %s: %v", root, closeErr)
			return
		}
		if fixture.retain {
			t.Logf("retained component fixture diagnostics: %s", root)
			return
		}
		if removeErr := os.RemoveAll(root); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.journal, err = openIntentJournal(t.Context(), fixture.store, true)
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	fixture.lease, acquired, err = fixture.journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("fixture election: %v", err)
	}
	fixture.intent = componentIntent(t, fixture.journal)
	changeComponentIntent(t, kind, &fixture.intent, fixture.journal)
	// Canonical direct publication also represents historical prior-writer data
	// for stale boot/native-pin refusal tests; it grants no child permission.
	err = fixture.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(intentName(fixture.intent.Sequence), fixture.intent)
		return putErr
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.reference, err = ReferenceStartup(fixture.intent)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *componentEntryFixture) Close() error {
	if f.lease != nil {
		if err := f.lease.Close(); err != nil {
			return err
		}
		f.lease = nil
	}
	if f.journal != nil {
		if err := f.journal.Close(); err != nil {
			return err
		}
		f.journal = nil
	}
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			return err
		}
		f.store = nil
	}
	return nil
}

func componentIntent(t *testing.T, journal *intentJournal) StartupIntent {
	t.Helper()
	intent := liveJournalIntent(t, journal)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable = componentExecutable(t, executable)
	intent.Inputs.Artifacts.Runner = executable
	record := contextForJournal(t, journal)
	record.OS = runtime.GOOS
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	intent.Inputs.Context = reference
	refreshIntentDigest(t, &intent)
	return intent
}

func changeComponentIntent(t *testing.T, kind string, intent *StartupIntent, journal *intentJournal) {
	t.Helper()
	switch kind {
	case "stale-boot":
		intent.Inputs.BootID = otherStableBoot(t, journal.bootID)
	case "stale-native-root":
		intent.Inputs.RootInode++
	case "wrong-executable":
		intent.Inputs.Artifacts.Runner = filepath.Join(journal.pin.Root(), "wrong-runner")
	case "expired":
		intent.Deadline = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
		intent.AcceptedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)

	}
	refreshIntentDigest(t, intent)
}

func runComponentEntryChild(t *testing.T, fixture *componentEntryFixture, mode string, arguments []string, wrongFile *os.File) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	childArgs := []string{"-test.run=^TestOwnerEntryComponentChild$", "-test.timeout=15s", "--", "--owner-entry-component-test", mode}
	childArgs = append(childArgs, arguments...)
	command := exec.Command(executable, childArgs...)
	if mode == "refuse-task-contained" {
		command.Env = append(os.Environ(), "DELEGATION_LAYER_PROCESS_SCOPE=pg:"+strings.Repeat("a", 32))
	}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	// Only exact Start runs under the parent descriptor borrow. The finite test
	// watchdog bounds the helper; its single Wait is always joined outside it.
	err = fixture.lease.WithHeldFile(t.Context(), func(file *os.File) error {
		if wrongFile != nil {
			file = wrongFile
		}
		command.ExtraFiles = []*os.File{file}
		return command.Start()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Wait(); err != nil {
		fixture.retain = true
		t.Fatalf("component child failed: %v\n%s", err, output.String())
	}
}

func componentArguments(t *testing.T, fixture *componentEntryFixture) []string {
	t.Helper()
	arguments, err := ownerArguments(fixture.root, fixture.store.RootID, fixture.reference)
	if err != nil {
		t.Fatal(err)
	}
	return arguments
}

func TestOwnerEntryComponentChild(t *testing.T) {
	mode, invocation, selected := componentChildInvocation(t)
	if !selected {
		return
	}
	var foreign *componentForeignDescriptor
	if mode == "refuse-foreign-descriptor" || mode == "wrong-descriptor" {
		foreign = ownComponentForeignDescriptor(t)
	}
	host := &componentEntryHost{reject: mode == "host-refusal"}
	ctx := t.Context()
	if mode == "refuse-nil-context" {
		checkComponentNilContextRefusal(t, invocation, host)
		return
	}
	entry, err := consumeOwnerInvocation(ctx, invocation, host)
	ownComponentEntry(t, entry)
	if componentRefusalMode(mode) {
		if foreign != nil {
			requireComponentForeignDescriptorPreserved(t, foreign)
			if !errors.Is(err, ErrIdentity) {
				t.Fatalf("foreign descriptor refusal lost identity cause: %v", err)
			}
		}
		if mode == "refuse-task-contained" {
			requireComponentContainmentRefusal(t, entry, err, host.calls)
		}
		requireComponentBirthRefusal(t, entry, err, host.calls, mode)
		return
	}
	if err != nil || entry == nil || entry.grant == nil {
		t.Fatalf("component birth: %v", err)
	}
	exerciseResidentComponent(t, entry, mode)
}

func checkComponentNilContextRefusal(t *testing.T, invocation ownerInvocation, host *componentEntryHost) {
	t.Helper()
	descriptor := ownComponentForeignDescriptor(t)
	arguments, err := ownerArguments(invocation.root, invocation.rootID, invocation.startup)
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context // Exercise the public boundary before FD3 adoption.
	handled, err := RunOwnerMode(ctx, arguments, host)
	if !handled || !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil context owner mode refusal: handled=%t error=%v", handled, err)
	}
	requireComponentForeignDescriptorPreserved(t, descriptor)
	requireComponentBirthRefusal(t, nil, err, host.calls, "refuse-nil-context")
}

func componentChildInvocation(t *testing.T) (string, ownerInvocation, bool) {
	t.Helper()
	index := slices.Index(os.Args, "--owner-entry-component-test")
	if index < 0 {
		return "", ownerInvocation{}, false
	}
	if len(os.Args) <= index+2 {
		t.Fatal("missing component invocation")
	}
	mode := os.Args[index+1]
	invocation, handled, err := parseOwnerInvocation(os.Args[index+2:])
	if err != nil || !handled {
		t.Fatalf("child invocation: %v", err)
	}
	if mode == "bare-descriptor" {
		file := os.NewFile(3, "owned-component-inherited-lease")
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	return mode, invocation, true
}

func ownComponentEntry(t *testing.T, entry *ownedResidentEntry) {
	t.Helper()
	// Ownership precedes error handling, including unresolved failed birth.
	if entry != nil {
		t.Cleanup(func() {
			if closeErr := entry.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
}

func (e *ownedResidentEntry) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), endpointCleanupTimeout)
	defer cancel()
	return e.closeContext(ctx)
}

func componentRefusalMode(mode string) bool {
	return strings.HasPrefix(mode, "refuse-") || mode == "host-refusal" || mode == "bare-descriptor" || mode == "wrong-descriptor"
}

func requireComponentBirthRefusal(t *testing.T, entry *ownedResidentEntry, err error, hostCalls int, mode string) {
	t.Helper()
	if err == nil || entry != nil {
		t.Fatalf("invalid component birth accepted: %v", err)
	}
	if mode == "refuse-valid-upgrade" && (hostCalls != 1 || !errors.Is(err, ErrOwnerIncompatible)) {
		t.Fatalf("valid upgrade did not reach pre-publication refusal: hostCalls=%d err=%v", hostCalls, err)
	}
	if mode != "host-refusal" && mode != "refuse-existing-assumption" && mode != "refuse-valid-upgrade" && hostCalls != 0 {
		t.Fatal("invalid passive identity reached host callback")
	}
}

func TestOwnerEntryConsumesNativeInheritedLease(t *testing.T) {
	fixture := newComponentEntryFixture(t, "")
	runComponentEntryChild(t, fixture, "success", componentArguments(t, fixture), nil)
	assumption, err := fixture.journal.readAssumption(t.Context(), fixture.reference)
	if err != nil || assumption.Process.PID == os.Getpid() {
		t.Fatalf("child did not own actual self assumption: %v", err)
	}
	if _, err = fixture.journal.readPublication(t.Context(), fixture.reference); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(ownerEndpointPath(fixture.root, fixture.intent.Sequence)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child left endpoint: %v", err)
	}
}

func TestOwnerEntryRefusesBeforeAssumptionAndSocket(t *testing.T) {
	for _, kind := range []string{"stale-boot", "stale-native-root", "wrong-executable", "expired", "root-id", "reference", "missing-root", "host-refusal", "bare-descriptor", "wrong-descriptor", "nil-context"} {
		t.Run(kind, func(t *testing.T) { checkComponentEntryRefusal(t, kind) })
	}
}

func checkComponentEntryRefusal(t *testing.T, kind string) {
	t.Helper()
	fixture := newComponentEntryFixture(t, kind)
	arguments := componentArguments(t, fixture)
	mode := "refuse-" + kind
	var wrongFile *os.File
	switch kind {
	case "root-id":
		arguments[4] = strings.Repeat("e", 32)
	case "reference":
		arguments[10] = strings.Repeat("e", 64)
	case "missing-root":
		if err := os.Remove(filepath.Join(fixture.root, "root.json")); err != nil {
			t.Fatal(err)
		}
	case "host-refusal", "bare-descriptor":
		mode = kind
	case "wrong-descriptor":
		mode = kind
		var err error
		wrongFile, err = os.OpenFile(filepath.Join(fixture.root, "wrong-lease"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := wrongFile.Close(); closeErr != nil {
				fixture.retain = true
				t.Error(closeErr)
			}
		})
	}
	runComponentEntryChild(t, fixture, mode, arguments, wrongFile)
	for _, name := range []string{filepath.Join("infrastructure", assumptionName(1)), filepath.Join("infrastructure", publicationName(1)), filepath.Base(ownerEndpointPath(fixture.root, fixture.intent.Sequence))} {
		if _, err := os.Lstat(filepath.Join(fixture.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refusal created %s: %v", name, err)
		}
	}
}

func TestOwnerEntryExistingAssumptionCannotBeAdopted(t *testing.T) {
	fixture := newComponentEntryFixture(t, "")
	assumption, _ := evidenceForIntent(t, fixture.intent)
	if created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, assumption); err != nil || !created {
		t.Fatalf("fixture assumption: %v", err)
	}
	runComponentEntryChild(t, fixture, "refuse-existing-assumption", componentArguments(t, fixture), nil)
	if _, err := os.Lstat(ownerEndpointPath(fixture.root, fixture.intent.Sequence)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate assumption created socket: %v", err)
	}
}

// Compile-time structural contracts keep this component independent of parent
// supervisor and app packages.
var _ EntryHost = (*componentEntryHost)(nil)

func requireComponentContainmentRefusal(t *testing.T, entry *ownedResidentEntry, err error, hostCalls int) {
	t.Helper()
	if !errors.Is(err, lifetime.ErrTaskContained) || entry != nil || hostCalls != 0 {
		t.Fatalf("contained task reached resident authority: entry=%t host-calls=%d error=%v", entry != nil, hostCalls, err)
	}
	if _, fdErr := unix.FcntlInt(3, unix.F_GETFD, 0); !errors.Is(fdErr, unix.EBADF) {
		t.Fatalf("contained entry retained inherited FD3: %v", fdErr)
	}
}

func TestOwnerEntryRejectsContainedTaskBeforeAuthority(t *testing.T) {
	fixture := newComponentEntryFixture(t, "task-contained")
	runComponentEntryChild(t, fixture, "refuse-task-contained", componentArguments(t, fixture), nil)
	for _, name := range []string{filepath.Join("infrastructure", assumptionName(1)), filepath.Join("infrastructure", publicationName(1)), filepath.Base(ownerEndpointPath(fixture.root, fixture.intent.Sequence))} {
		if _, err := os.Lstat(filepath.Join(fixture.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("contained task created authority resource %s: %v", name, err)
		}
	}
	if err := fixture.lease.Close(); err != nil {
		fixture.retain = true
		t.Fatal(err)
	}
	fixture.lease = nil
	var lease *Lease
	t.Cleanup(func() {
		if lease != nil {
			if closeErr := lease.Close(); closeErr != nil {
				fixture.retain = true
				t.Error(closeErr)
			}
		}
	})
	var acquired bool
	var err error
	lease, acquired, err = TryAcquireLease(t.Context(), filepath.Join(fixture.root, instanceLeaseName))
	if err != nil || !acquired {
		t.Fatalf("contained child did not release instance reference: acquired=%t error=%v", acquired, err)
	}
}

type componentForeignDescriptor struct {
	file   *os.File
	stat   unix.Stat_t
	flags  int
	status int
}

func ownComponentForeignDescriptor(t *testing.T) *componentForeignDescriptor {
	t.Helper()
	snapshot := &componentForeignDescriptor{file: os.NewFile(3, "component-owned-foreign-descriptor")}
	t.Cleanup(func() {
		if closeErr := snapshot.file.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) && !errors.Is(closeErr, unix.EBADF) {
			t.Error(closeErr)
		}
	})
	var err error
	snapshot.flags, err = unix.FcntlInt(3, unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.status, err = unix.FcntlInt(3, unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Fstat(3, &snapshot.stat); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func requireComponentForeignDescriptorPreserved(t *testing.T, snapshot *componentForeignDescriptor) {
	t.Helper()
	flags, err := unix.FcntlInt(3, unix.F_GETFD, 0)
	if err != nil || flags != snapshot.flags {
		t.Fatalf("foreign descriptor was closed or flags changed: %v", err)
	}
	status, err := unix.FcntlInt(3, unix.F_GETFL, 0)
	if err != nil || status != snapshot.status {
		t.Fatalf("foreign descriptor status flags changed: %v", err)
	}
	var current unix.Stat_t
	if err = unix.Fstat(3, &current); err != nil {
		t.Fatal(err)
	}
	if current.Dev != snapshot.stat.Dev || current.Ino != snapshot.stat.Ino || current.Mode != snapshot.stat.Mode {
		t.Fatal("foreign descriptor identity changed")
	}
}

func TestOwnerEntryPreservesForeignInheritedDescriptor(t *testing.T) {
	fixture := newComponentEntryFixture(t, "foreign-descriptor")
	foreign, err := os.OpenFile(filepath.Join(fixture.root, "foreign-file"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := foreign.Close(); closeErr != nil {
			fixture.retain = true
			t.Error(closeErr)
		}
	})
	runComponentEntryChild(t, fixture, "refuse-foreign-descriptor", componentArguments(t, fixture), foreign)
	for _, name := range []string{filepath.Join("infrastructure", assumptionName(1)), filepath.Join("infrastructure", publicationName(1)), filepath.Base(ownerEndpointPath(fixture.root, fixture.intent.Sequence))} {
		if _, err = os.Lstat(filepath.Join(fixture.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("foreign descriptor created authority resource %s: %v", name, err)
		}
	}
}

func TestOwnerEntryFixturePreflightChild(t *testing.T) {
	if !slices.Contains(os.Args, "--owner-entry-preflight-test") {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Deferred code observes the directory before testing runs fixture Cleanup.
	defer func() {
		entries, readErr := os.ReadDir(home)
		if readErr != nil {
			t.Error(readErr)
			return
		}
		if len(entries) != 0 {
			t.Error("preflight created fixture resources before refusal")
		}
	}()
	newComponentEntryFixture(t, "")
	t.Fatal("contained success fixture was not blocked")
}

func TestOwnerEntryFixturePreflightCreatesNoResources(t *testing.T) {
	home := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestOwnerEntryFixturePreflightChild$", "-test.timeout=15s", "--", "--owner-entry-preflight-test")
	command.Env = append(os.Environ(), "HOME="+home, "DELEGATION_LAYER_PROCESS_SCOPE=pg:"+strings.Repeat("a", 32))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err = command.Run() // Owns this finite helper's exact Start and joined Wait.
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || !strings.Contains(output.String(), "blocked: test host Delegate-contained") || strings.Contains(output.String(), "preflight created fixture resources") {
		t.Fatalf("fixture did not block before creating resources: %v\n%s", err, output.String())
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("blocked fixture left resources: %v", err)
	}
}

func requireComponentEntryTestHost(t *testing.T) {
	t.Helper()
	err := lifetime.CheckCurrentTaskContainment(t.Context(), os.Environ())
	if err == nil {
		return
	}
	if errors.Is(err, lifetime.ErrTaskContained) {
		t.Fatal("blocked: test host Delegate-contained")
	}
	t.Fatalf("blocked: test host containment observation failed: %v", err)
}

func componentExecutable(t *testing.T, executable string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestOwnerEntryFixtureCanonicalizesExecutableAlias(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "test-image")
	if err = os.Symlink(canonical, alias); err != nil {
		t.Fatal(err)
	}
	if resolved := componentExecutable(t, alias); resolved != canonical {
		t.Fatal("fixture did not select the installed native path")
	}
	if err = validateExecutingRunnerPath(alias, canonical); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(directory, "other-installed-runner")
	if err = os.WriteFile(other, []byte("original fixture source\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = validateExecutingRunnerPath(alias, other); !errors.Is(err, ErrIdentity) {
		t.Fatalf("different installed target accepted: %v", err)
	}
}
