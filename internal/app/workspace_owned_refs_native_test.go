package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// A symbolic ref at a Delegate-owned path is never Delegate's own record. A
// dangling one is omitted by native ref inventories, so create-only pins and
// expected-OID cleanup must refuse it as unreadable and leave it unchanged
// rather than replace it or treat it as absent. These cases run the public
// workspace.Git port over native Git through the nativeLineage launch
// boundary; they are not a Pueue-worker or provider composition.
func TestNativeOwnedRefsRefuseDanglingSymbolicRefs(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			lineage := newNativeLineageFormat(t, format)
			repository := newOwnedRefRepository(t, lineage, lineage.main)
			t.Run("pin base", repository.requirePinBase)
			t.Run("pin capture", repository.requirePinCapture)
			t.Run("delete refs", repository.requireDeleteRefs)
			if format == "sha1" {
				t.Run("reftable", func(t *testing.T) { requireReftableOwnedRefs(t, lineage) })
			}
		})
	}
}

// ownedRefRepository is one native repository with its resolved source and
// the ordinary state owned-ref operations must never change.
type ownedRefRepository struct {
	lineage  nativeLineage
	dir      string
	source   workspace.Source
	ordinary ordinaryGitState
}

type ordinaryGitState struct {
	head, branch, indexSHA256 string
}

func newOwnedRefRepository(t *testing.T, lineage nativeLineage, dir string) ownedRefRepository {
	t.Helper()
	source, err := lineage.git.ResolveSource(lineage.scope.ctx, dir, "")
	if err != nil {
		t.Fatalf("resolve native source: %v", err)
	}
	repository := ownedRefRepository{lineage: lineage, dir: dir, source: source}
	repository.ordinary = repository.observeOrdinary(t)
	return repository
}

func (r ownedRefRepository) observeOrdinary(t *testing.T) ordinaryGitState {
	t.Helper()
	index := r.lineage.gitIn(t, r.dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	return ordinaryGitState{
		head:        r.lineage.gitIn(t, r.dir, "rev-parse", "--verify", "HEAD"),
		branch:      r.lineage.gitIn(t, r.dir, "symbolic-ref", "HEAD"),
		indexSHA256: task.ComputeSHA256(data),
	}
}

func (r ownedRefRepository) requireOrdinaryUnchanged(t *testing.T) {
	t.Helper()
	if current := r.observeOrdinary(t); current != r.ordinary {
		t.Errorf("owned-ref operation changed ordinary HEAD/index state: %+v, want %+v", current, r.ordinary)
	}
}

// newOwnedRef returns a fresh owned workspace prefix and its base ref.
func newOwnedRef(t *testing.T) (prefix, ref string) {
	t.Helper()
	rootID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	prefix = "refs/delegate/" + rootID + "/workspaces/" + workspaceID + "/"
	return prefix, prefix + "base"
}

// danglingOwnedRef makes ref a symbolic ref to an absent ref inside the same
// owned prefix, so no real branch is ever a write target.
func (r ownedRefRepository) danglingOwnedRef(t *testing.T) (prefix, ref, target string) {
	t.Helper()
	prefix, ref = newOwnedRef(t)
	target = prefix + "missing-target"
	r.lineage.gitIn(t, r.dir, "symbolic-ref", ref, target)
	return prefix, ref, target
}

// requireDanglingPreserved reads the ref back natively: it must still be a
// symbolic ref to target, and target must still be absent.
func (r ownedRefRepository) requireDanglingPreserved(t *testing.T, ref, target string) {
	t.Helper()
	symbolic, err := r.lineage.gitExitsZero(r.dir, "symbolic-ref", "--quiet", "--no-recurse", ref)
	if err != nil {
		t.Fatal(err)
	}
	if !symbolic {
		t.Errorf("dangling symbolic ref %s was replaced", ref)
		return
	}
	if got := r.lineage.gitIn(t, r.dir, "symbolic-ref", "--quiet", "--no-recurse", ref); got != target {
		t.Errorf("symbolic ref %s now points to %s, want %s", ref, got, target)
	}
	if r.lineage.gitSucceeds(t, r.dir, "show-ref", "--verify", "--quiet", target) {
		t.Errorf("dangling target %s was created", target)
	}
}

// requireDirectRef reads ref back natively as a direct ref to want.
func (r ownedRefRepository) requireDirectRef(t *testing.T, ref, want string) {
	t.Helper()
	if r.lineage.gitSucceeds(t, r.dir, "symbolic-ref", "--quiet", "--no-recurse", ref) {
		t.Errorf("owned ref %s is symbolic, want a direct ref", ref)
	}
	if got := r.lineage.gitIn(t, r.dir, "rev-parse", "--verify", ref); got != want {
		t.Errorf("owned ref %s points at %s, want %s", ref, got, want)
	}
}

func (r ownedRefRepository) requireAbsentRef(t *testing.T, ref string) {
	t.Helper()
	if r.lineage.gitSucceeds(t, r.dir, "symbolic-ref", "--quiet", "--no-recurse", ref) ||
		r.lineage.gitSucceeds(t, r.dir, "show-ref", "--verify", "--quiet", ref) {
		t.Errorf("owned ref %s still exists", ref)
	}
}

func (r ownedRefRepository) requirePinBase(t *testing.T) {
	ctx := r.lineage.scope.ctx
	_, created := newOwnedRef(t)
	if err := r.lineage.git.PinBase(ctx, r.source, created); err != nil {
		t.Fatalf("pin absent base ref: %v", err)
	}
	if err := r.lineage.git.PinBase(ctx, r.source, created); err != nil {
		t.Errorf("replay of the same base pin: %v", err)
	}
	r.requireDirectRef(t, created, r.source.BaseOID)
	_, conflicting := newOwnedRef(t)
	other := r.otherCommit(t)
	r.lineage.gitIn(t, r.dir, "update-ref", conflicting, other)
	if err := r.lineage.git.PinBase(ctx, r.source, conflicting); !errors.Is(err, workspace.ErrRefConflict) {
		t.Errorf("pin over a different direct ref: %v, want ErrRefConflict", err)
	}
	r.requireDirectRef(t, conflicting, other)
	_, dangling, target := r.danglingOwnedRef(t)
	if err := r.lineage.git.PinBase(ctx, r.source, dangling); !errors.Is(err, workspace.ErrRefUnreadable) {
		t.Errorf("pin over a dangling symbolic ref: %v, want ErrRefUnreadable", err)
	}
	r.requireDanglingPreserved(t, dangling, target)
	r.requireOrdinaryUnchanged(t)
}

// otherCommit writes a commit that is not HEAD without moving any ordinary ref.
func (r ownedRefRepository) otherCommit(t *testing.T) string {
	t.Helper()
	tree := r.lineage.gitIn(t, r.dir, "rev-parse", "--verify", "HEAD^{tree}")
	return r.lineage.gitIn(t, r.dir, "-c", "user.name=Delegate fixture", "-c", "user.email=fixture@example.invalid",
		"commit-tree", tree, "-p", r.source.BaseOID, "-m", "conflicting owned ref")
}

// The capture pin shares the create-only path through its own public entry.
func (r ownedRefRepository) requirePinCapture(t *testing.T) {
	ctx := r.lineage.scope.ctx
	checkout, err := r.lineage.git.Provision(ctx, r.source, filepath.Join(r.lineage.root, "owned-ref-capture"))
	if err != nil {
		t.Fatalf("provision capture checkout: %v", err)
	}
	scratch := filepath.Join(r.lineage.root, "owned-ref-capture-scratch")
	if err = os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate, err := r.lineage.git.BuildCapture(ctx, checkout, scratch)
	if err != nil {
		t.Fatalf("build capture candidate: %v", err)
	}
	_, created := newOwnedRef(t)
	if err = r.lineage.git.PinCapture(ctx, checkout, created, candidate); err != nil {
		t.Fatalf("pin absent capture ref: %v", err)
	}
	r.requireDirectRef(t, created, candidate.TreeOID)
	_, dangling, target := r.danglingOwnedRef(t)
	if err = r.lineage.git.PinCapture(ctx, checkout, dangling, candidate); !errors.Is(err, workspace.ErrRefUnreadable) {
		t.Errorf("capture pin over a dangling symbolic ref: %v, want ErrRefUnreadable", err)
	}
	r.requireDanglingPreserved(t, dangling, target)
	r.requireOrdinaryUnchanged(t)
}

func (r ownedRefRepository) requireDeleteRefs(t *testing.T) {
	ctx := r.lineage.scope.ctx
	expected := func(ref string) []workspace.ForgetRef {
		return []workspace.ForgetRef{{Ref: ref, ExpectedOID: r.source.BaseOID}}
	}
	absentPrefix, absent := newOwnedRef(t)
	if err := r.lineage.git.DeleteRefs(ctx, r.source, absentPrefix, expected(absent)); err != nil {
		t.Errorf("replay of an already absent expected ref: %v", err)
	}
	directPrefix, direct := newOwnedRef(t)
	r.lineage.gitIn(t, r.dir, "update-ref", direct, r.source.BaseOID)
	if err := r.lineage.git.DeleteRefs(ctx, r.source, directPrefix, expected(direct)); err != nil {
		t.Errorf("delete the expected direct ref: %v", err)
	}
	r.requireAbsentRef(t, direct)
	danglingPrefix, dangling, target := r.danglingOwnedRef(t)
	if err := r.lineage.git.DeleteRefs(ctx, r.source, danglingPrefix, expected(dangling)); !errors.Is(err, workspace.ErrRefUnreadable) {
		t.Errorf("delete over a dangling symbolic ref: %v, want ErrRefUnreadable", err)
	}
	r.requireDanglingPreserved(t, dangling, target)
	r.requireOrdinaryUnchanged(t)
}

// requireReftableOwnedRefs repeats the dangling refusals on a reftable
// repository when this native Git accepts reftable initialization. A refused
// probe means only that this Git does not exercise the backend.
func requireReftableOwnedRefs(t *testing.T, lineage nativeLineage) {
	dir := filepath.Join(lineage.root, "reftable")
	accepted, err := lineage.gitExitsZero(lineage.root, "init", "--quiet", "--object-format=sha1", "--ref-format=reftable", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Log("native Git refused reftable initialization; reftable owned refs not exercised on this host")
		return
	}
	lineage.commit(t, dir, "reftable base")
	repository := newOwnedRefRepository(t, lineage, dir)
	ctx := lineage.scope.ctx
	_, pinned, pinnedTarget := repository.danglingOwnedRef(t)
	if err = lineage.git.PinBase(ctx, repository.source, pinned); !errors.Is(err, workspace.ErrRefUnreadable) {
		t.Errorf("reftable pin over a dangling symbolic ref: %v, want ErrRefUnreadable", err)
	}
	repository.requireDanglingPreserved(t, pinned, pinnedTarget)
	prefix, deleted, deletedTarget := repository.danglingOwnedRef(t)
	forget := []workspace.ForgetRef{{Ref: deleted, ExpectedOID: repository.source.BaseOID}}
	if err = lineage.git.DeleteRefs(ctx, repository.source, prefix, forget); !errors.Is(err, workspace.ErrRefUnreadable) {
		t.Errorf("reftable delete over a dangling symbolic ref: %v, want ErrRefUnreadable", err)
	}
	repository.requireDanglingPreserved(t, deleted, deletedTarget)
	repository.requireOrdinaryUnchanged(t)
}

// Several inventory-missing names exercise the whole native absence proof
// through the public DeleteRefs entry. The first update-ref batch is the
// capability observation: a Git that accepts symref-verify must verify the
// remaining names in one second batch, and a Git that refuses it must take
// only the files-backend fallback. The route comes from observed behavior,
// never from the Git version, which is logged only as receipt evidence.
func TestNativeOwnedRefsProveSeveralAbsentNames(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			lineage := newNativeLineageFormat(t, format)
			recorder := &ownedRefCommandRecorder{next: lineage.git.Runner}
			observed := lineage
			observed.git.Runner = recorder
			repository := newOwnedRefRepository(t, observed, observed.main)
			refsBefore := repository.refInventory(t)
			prefix, _ := newOwnedRef(t)
			names := []string{prefix + "absent-a", prefix + "absent-b", prefix + "absent-c"}
			forget := make([]workspace.ForgetRef, 0, len(names))
			for _, name := range names {
				forget = append(forget, workspace.ForgetRef{Ref: name, ExpectedOID: repository.source.BaseOID})
			}
			mark := recorder.mark()
			if err := observed.git.DeleteRefs(observed.scope.ctx, repository.source, prefix, forget); err != nil {
				t.Fatalf("replay of three absent expected refs: %v", err)
			}
			route := requireOwnedRefAbsenceRoute(t, recorder.since(mark), repository.source.CommonDir, names)
			t.Logf("native %s; owned-ref absence route: %s",
				lineage.gitIn(t, observed.main, "--version"), route)
			for _, name := range names {
				repository.requireAbsentRef(t, name)
			}
			if refsAfter := repository.refInventory(t); refsAfter != refsBefore {
				t.Errorf("absence proof changed refs:\n%s\nwant:\n%s", refsAfter, refsBefore)
			}
			repository.requireOrdinaryUnchanged(t)
			requireNoRefLockResidue(t, repository.source.CommonDir)
		})
	}
}

const (
	ownedRefAbsenceAck         = "start: ok\nprepare: ok\nabort: ok\n"
	ownedRefAbsenceUnsupported = "start: ok\n"
	ownedRefRouteNative        = "native symref-verify batches of 1 then 2"
	ownedRefRouteFallback      = "files-backend fallback after unsupported symref-verify"
)

// requireOwnedRefAbsenceRoute checks every update-ref and ref-storage query
// the replay launched and returns the route its first batch selected.
func requireOwnedRefAbsenceRoute(t *testing.T, commands []ownedRefCommand, commonDir string, names []string) string {
	t.Helper()
	updates := ownedRefCommandsWith(commands, "update-ref")
	storage := ownedRefCommandsWith(commands, "extensions.refStorage")
	if len(updates) == 0 {
		t.Fatal("absence proof launched no update-ref batch")
	}
	t.Logf("resolved Git executable %s", updates[0].path)
	first := updates[0]
	requireOwnedRefAbsenceBatch(t, first, commonDir, names[:1])
	switch {
	case first.err == nil && first.stdout == ownedRefAbsenceAck && first.stderr == "":
		if len(updates) != 2 || len(storage) != 0 {
			t.Fatalf("native route launched %d update-ref batches and %d ref-storage queries, want 2 and 0", len(updates), len(storage))
		}
		second := updates[1]
		requireOwnedRefAbsenceBatch(t, second, commonDir, names[1:])
		if second.err != nil || second.stdout != ownedRefAbsenceAck || second.stderr != "" {
			t.Fatalf("second batch = %v stdout %q stderr %q, want clean %q", second.err, second.stdout, second.stderr, ownedRefAbsenceAck)
		}
		return ownedRefRouteNative
	case ownedRefExitCode(first.err) == 128 && first.stdout == ownedRefAbsenceUnsupported &&
		first.stderr == "fatal: unknown command: symref-verify "+names[0]+"\n":
		if len(updates) != 1 || len(storage) != 1 {
			t.Fatalf("fallback route launched %d update-ref batches and %d ref-storage queries, want 1 and 1", len(updates), len(storage))
		}
		requireOwnedRefStorageQuery(t, storage[0], commonDir)
		return ownedRefRouteFallback
	default:
		t.Fatalf("first batch = %v stdout %q stderr %q; neither the native acknowledgement nor the exact unsupported refusal",
			first.err, first.stdout, first.stderr)
		return ""
	}
}

func requireOwnedRefAbsenceBatch(t *testing.T, command ownedRefCommand, commonDir string, refs []string) {
	t.Helper()
	wantArgs := []string{"-C", commonDir, "update-ref", "--stdin", "-z", "--no-deref"}
	if !slices.Equal(command.args, wantArgs) {
		t.Fatalf("absence batch argv = %q, want %q", command.args, wantArgs)
	}
	want := "start\x00"
	for _, ref := range refs {
		want += "symref-verify " + ref + "\x00\x00"
	}
	want += "prepare\x00abort\x00"
	if string(command.stdin) != want {
		t.Fatalf("absence batch input = %q, want %q", command.stdin, want)
	}
}

// requireOwnedRefStorageQuery checks the fallback's one query of the common
// directory's own configuration file, which this fixture leaves unset.
func requireOwnedRefStorageQuery(t *testing.T, command ownedRefCommand, commonDir string) {
	t.Helper()
	wantArgs := []string{"-C", commonDir, "config", "--file=" + filepath.Join(commonDir, "config"), "--get-all", "extensions.refStorage"}
	if !slices.Equal(command.args, wantArgs) {
		t.Fatalf("ref-storage query argv = %q, want %q", command.args, wantArgs)
	}
	if ownedRefExitCode(command.err) != 1 || command.stdout != "" || command.stderr != "" {
		t.Fatalf("ref-storage query = %v stdout %q stderr %q, want a clean unset exit 1", command.err, command.stdout, command.stderr)
	}
}

// ownedRefExitCode returns an ordinary Git exit status, or -1 for any other
// result.
func ownedRefExitCode(err error) int {
	var exit *exec.ExitError
	if !ordinaryGitExit(err) || !errors.As(err, &exit) {
		return -1
	}
	return exit.ExitCode()
}

// ownedRefCommandsWith selects commands with argument as one whole argv
// element, so a directory path that merely contains it does not match.
func ownedRefCommandsWith(commands []ownedRefCommand, argument string) []ownedRefCommand {
	var matched []ownedRefCommand
	for _, command := range commands {
		if slices.Contains(command.args, argument) {
			matched = append(matched, command)
		}
	}
	return matched
}

func TestOwnedRefCommandsWithMatchesWholeArguments(t *testing.T) {
	commonDir := "/tmp/update-ref/extensions.refStorage/common"
	tests := []struct {
		name, argument string
		args           []string
		want           int
	}{
		{name: "update-ref in a path", argument: "update-ref", args: []string{"-C", commonDir, "symbolic-ref", "HEAD"}},
		{name: "update-ref subcommand", argument: "update-ref", args: []string{"-C", commonDir, "update-ref", "--stdin", "-z", "--no-deref"}, want: 1},
		{
			name: "ref-storage key", argument: "extensions.refStorage", want: 1,
			args: []string{"-C", commonDir, "config", "--file=" + commonDir + "/config", "--get-all", "extensions.refStorage"},
		},
		{name: "ref-storage key in a path", argument: "extensions.refStorage", args: []string{"-C", commonDir, "config", "--file=" + commonDir + "/config", "--get-all", "core.bare"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := len(ownedRefCommandsWith([]ownedRefCommand{{args: test.args}}, test.argument)); got != test.want {
				t.Fatalf("%s matched %d commands, want %d", test.argument, got, test.want)
			}
		})
	}
}

// refInventory lists every ref with its target and symbolic target.
func (r ownedRefRepository) refInventory(t *testing.T) string {
	t.Helper()
	return r.lineage.gitIn(t, r.dir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
}

// requireNoRefLockResidue refuses any lock file the verify-only batches could
// leave under the common directory.
func requireNoRefLockResidue(t *testing.T, commonDir string) {
	t.Helper()
	err := filepath.WalkDir(commonDir, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.HasSuffix(path, ".lock") {
			t.Errorf("lock residue %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ownedRefCommand is one observed launch: its argv after the executable,
// stdin, captured output and run result. The environment is not recorded.
type ownedRefCommand struct {
	path, stdout, stderr string
	args                 []string
	stdin                []byte
	err                  error
}

// ownedRefCommandRecorder observes Git launches made through the lineage's
// existing runner. It copies stdin and tees captured output; the wrapped
// runner still owns Start, Wait, scope and cleanup.
type ownedRefCommandRecorder struct {
	next     workspace.CommandRunner
	mu       sync.Mutex
	commands []ownedRefCommand
}

func (r *ownedRefCommandRecorder) Run(ctx context.Context, command *exec.Cmd) error {
	observed := ownedRefCommand{path: command.Path, args: slices.Clone(command.Args[1:])}
	if command.Stdin != nil {
		input, err := io.ReadAll(command.Stdin)
		if err != nil {
			return err
		}
		observed.stdin = input
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	// A file destination keeps its descriptor; only buffered output is teed.
	if _, file := command.Stdout.(*os.File); !file && command.Stdout != nil {
		command.Stdout = io.MultiWriter(command.Stdout, &stdout)
	}
	if _, file := command.Stderr.(*os.File); !file && command.Stderr != nil {
		command.Stderr = io.MultiWriter(command.Stderr, &stderr)
	}
	err := r.next.Run(ctx, command)
	observed.stdout, observed.stderr, observed.err = stdout.String(), stderr.String(), err
	r.mu.Lock()
	r.commands = append(r.commands, observed)
	r.mu.Unlock()
	return err
}

func (r *ownedRefCommandRecorder) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.commands)
}

func (r *ownedRefCommandRecorder) since(mark int) []ownedRefCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.commands[mark:])
}
