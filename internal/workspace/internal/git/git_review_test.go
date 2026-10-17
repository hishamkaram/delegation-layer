package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestInspectCleanupIgnoresStaleRegistrationWithMissingParent(t *testing.T) {
	fixture := newCheckoutFixture(t)
	stalePath := filepath.Join(fixture.root, "removed-parent", "stale-worktree")
	if _, err := os.Stat(filepath.Dir(stalePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale registration parent = %v, want absent", err)
	}

	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"worktree", "list", "--porcelain", "-z"}) {
			output := "worktree " + fixture.root + "\x00HEAD " + testBaseOID + "\x00"
			output += "worktree " + stalePath + "\x00HEAD " + testHeadOID + "\x00"
			output += "worktree " + fixture.checkout.CheckoutPath + "\x00HEAD " + testBaseOID + "\x00locked\x00"
			return writeCommandOutput(command, output)
		}
		return baseHandler(command)
	}

	got, err := (Git{Runner: runner}).InspectCleanup(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if err != nil {
		t.Fatalf("inspect cleanup with stale registration: %v", err)
	}
	if got.CheckoutPath != fixture.checkout.CheckoutPath {
		t.Fatalf("checkout path = %q, want %q", got.CheckoutPath, fixture.checkout.CheckoutPath)
	}
}

func TestSourceIsDirtyDisablesOptionalIndexLocks(t *testing.T) {
	directory := canonicalWorkspaceTestDir(t)
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
		args := command.Args[3:]
		if !slices.Equal(args, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}) {
			return fmt.Errorf("unexpected Git command: %v", args)
		}
		return writeCommandOutput(command, " M tracked.txt\x00")
	}}

	dirty, err := (Git{Runner: runner}).sourceIsDirty(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("source dirtiness was not observed")
	}
	if len(runner.calls) != 1 || !slices.Contains(runner.calls[0].Env, "GIT_OPTIONAL_LOCKS=0") {
		t.Fatalf("status environment does not disable optional locks: %v", runner.calls)
	}
}

func TestDeleteRefsZeroInventoryDoesNotRunGit(t *testing.T) {
	runner := &recordingGitRunner{}
	err := (Git{Runner: runner}).DeleteRefs(context.Background(), Source{}, "refs/delegate/root/workspaces/one/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("zero-ref deletion launched %d Git commands", len(runner.calls))
	}
}

func TestDeleteRefsUsesOneInventoryAndTransactionalUpdate(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	prefix := "refs/delegate/root/workspaces/one/"
	refs := []ForgetRef{
		{Ref: prefix + "capture-b", ExpectedOID: testTreeOID},
		{Ref: prefix + "missing", ExpectedOID: testHeadOID},
		{Ref: prefix + "capture-a", ExpectedOID: testBaseOID},
	}
	inventory := refInventoryRecord(prefix+"capture-b", testTreeOID, "tree", "")
	inventory = append(inventory, refInventoryRecord(prefix+"capture-a", testBaseOID, "commit", "")...)
	inventory = append(inventory, refInventoryRecord(prefix+"unlisted", testHeadOID, "commit", "")...)
	runner := newDeleteRefsRunner(root, prefix, inventory, nil)
	if err := (Git{Runner: runner}).DeleteRefs(context.Background(), source, prefix, refs); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 6 {
		t.Fatalf("Git command count = %d, want source validation + inventory + transaction", len(runner.calls))
	}
	if len(runner.updateInputs) != 1 {
		t.Fatalf("update-ref transactions = %d, want 1", len(runner.updateInputs))
	}
	wantTransaction := []byte("start\x00delete " + prefix + "capture-a\x00" + testBaseOID + "\x00delete " + prefix + "capture-b\x00" + testTreeOID + "\x00prepare\x00commit\x00")
	if !bytes.Equal(runner.updateInputs[0], wantTransaction) {
		t.Fatalf("transaction = %q, want %q", runner.updateInputs[0], wantTransaction)
	}
	if !slices.Equal(runner.updateArgs, []string{"update-ref", "--stdin", "-z", "--no-deref"}) {
		t.Fatalf("update-ref args = %v", runner.updateArgs)
	}
}

func TestDeleteRefsRejectsConflictingOrUnreadableExpectedRefs(t *testing.T) {
	prefix := "refs/delegate/root/workspaces/one/"
	tests := []struct {
		name      string
		inventory []byte
		wantErr   error
	}{
		{
			name:      "conflict",
			inventory: refInventoryRecord(prefix+"capture", testHeadOID, "tree", ""),
			wantErr:   ErrRefConflict,
		},
		{
			name:      "symbolic",
			inventory: refInventoryRecord(prefix+"capture", testTreeOID, "tree", "refs/heads/main"),
			wantErr:   ErrRefUnreadable,
		},
		{
			name:      "invalid object",
			inventory: refInventoryRecord(prefix+"capture", "not-an-oid", "tree", ""),
			wantErr:   ErrRefUnreadable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := canonicalWorkspaceTestDir(t)
			gitDir := filepath.Join(root, ".git")
			if err := os.Mkdir(gitDir, 0o700); err != nil {
				t.Fatal(err)
			}
			source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
			runner := newDeleteRefsRunner(root, prefix, test.inventory, nil)
			err := (Git{Runner: runner}).DeleteRefs(context.Background(), source, prefix, []ForgetRef{{Ref: prefix + "capture", ExpectedOID: testTreeOID}})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if len(runner.updateInputs) != 0 {
				t.Fatal("invalid inventory reached update-ref")
			}
		})
	}
}

func TestDeleteRefsLargeReplayKeepsLaunchesAndArgvBounded(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	prefix := "refs/delegate/root/workspaces/large/"
	refs := make([]ForgetRef, 2000)
	var initialInventory []byte
	for index := range refs {
		ref := fmt.Sprintf("%scapture-%03d", prefix, index)
		refs[index] = ForgetRef{Ref: ref, ExpectedOID: testTreeOID}
		if index%4 != 0 {
			initialInventory = append(initialInventory, refInventoryRecord(ref, testTreeOID, "tree", "")...)
		}
	}
	if len(initialInventory) <= 64*1024 {
		t.Fatal("bulk-ref fixture does not exceed the scalar output bound")
	}
	runner := newDeleteRefsRunner(root, prefix, initialInventory, errors.New("simulated transaction interruption"))
	git := Git{Runner: runner}
	if err := git.DeleteRefs(context.Background(), source, prefix, refs); err == nil {
		t.Fatal("first transaction unexpectedly succeeded")
	}

	conflictRef := refs[1].Ref
	runner.inventory = refInventoryRecord(conflictRef, testHeadOID, "tree", "")
	runner.updateErr = nil
	if err := git.DeleteRefs(context.Background(), source, prefix, refs); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("replay conflict error = %v, want ErrRefConflict", err)
	}

	runner.inventory = nil
	if err := git.DeleteRefs(context.Background(), source, prefix, refs); err != nil {
		t.Fatalf("replay with authoritatively absent refs: %v", err)
	}
	if len(runner.calls) > 64 {
		t.Fatalf("Git launches = %d, want <= 64", len(runner.calls))
	}
	if len(runner.updateInputs) != 1 || bytes.Count(runner.updateInputs[0], []byte("delete ")) != 1500 {
		t.Fatalf("transaction delete records = %d, want 1500", bytes.Count(runner.updateInputs[0], []byte("delete ")))
	}
	for _, command := range runner.calls {
		if len(command.Args[3:]) > 4 {
			t.Fatalf("Git command grew per-ref argv: %v", command.Args[3:])
		}
	}
}

type deleteRefsTestRunner struct {
	*recordingGitRunner
	inventory    []byte
	updateErr    error
	updateInputs [][]byte
	updateArgs   []string
}

func newDeleteRefsRunner(root, prefix string, inventory []byte, updateErr error) *deleteRefsTestRunner {
	runner := &deleteRefsTestRunner{inventory: inventory, updateErr: updateErr}
	runner.recordingGitRunner = &recordingGitRunner{}
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		switch {
		case slices.Equal(args, []string{"rev-parse", "--show-toplevel"}):
			return writeCommandOutput(command, root+"\n")
		case slices.Equal(args, []string{"rev-parse", "--git-dir"}), slices.Equal(args, []string{"rev-parse", "--git-common-dir"}):
			return writeCommandOutput(command, ".git\n")
		case slices.Equal(args, []string{"rev-parse", "--show-object-format=storage"}):
			return writeCommandOutput(command, "sha1\n")
		case slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, prefix}):
			_, err := command.Stdout.Write(runner.inventory)
			return err
		case slices.Equal(args, []string{"update-ref", "--stdin", "-z", "--no-deref"}):
			data, err := io.ReadAll(command.Stdin)
			if err != nil {
				return err
			}
			runner.updateInputs = append(runner.updateInputs, data)
			runner.updateArgs = slices.Clone(args)
			return runner.updateErr
		default:
			return fmt.Errorf("unexpected Git command: %v", args)
		}
	}
	return runner
}

func refInventoryRecord(ref, objectName, objectType, symref string) []byte {
	return []byte(ref + "\x00" + objectName + "\x00" + objectType + "\x00" + symref + "\x00\n")
}

func TestParseOwnedRefInventoryRequiresNativeRecordFraming(t *testing.T) {
	ref := "refs/delegate/root/workspaces/one/base"
	valid := refInventoryRecord(ref, testBaseOID, "commit", "")
	for _, data := range [][]byte{valid[:len(valid)-1], append(slices.Clone(valid), '\n'), []byte(ref + "\x00")} {
		if _, err := parseOwnedRefInventory(data); err == nil {
			t.Fatalf("malformed inventory %q was accepted", data)
		}
	}
	inventory, err := parseOwnedRefInventory(append(slices.Clone(valid), refInventoryRecord(ref+"-second", testTreeOID, "tree", "")...))
	if err != nil || len(inventory) != 2 {
		t.Fatalf("native inventory = %v, error = %v", inventory, err)
	}
}

func TestDeleteRefsDoesNotTreatGitWarningsAsAbsence(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prefix := "refs/delegate/root/workspaces/one/"
	runner := newDeleteRefsRunner(root, prefix, nil, nil)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		if command.Args[3] == "for-each-ref" {
			_, err := io.WriteString(command.Stderr, "warning: ignoring broken ref "+prefix+"capture\n")
			return err
		}
		return baseHandler(command)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	err := (Git{Runner: runner}).DeleteRefs(context.Background(), source, prefix, []ForgetRef{{Ref: prefix + "capture", ExpectedOID: testTreeOID}})
	if !errors.Is(err, ErrRefUnreadable) || len(runner.updateInputs) != 0 {
		t.Fatalf("warning was not preserved: error = %v, transactions = %d", err, len(runner.updateInputs))
	}
}
