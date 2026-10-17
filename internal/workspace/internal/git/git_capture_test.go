package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildCaptureUsesPrivateIndexAndPreservesExactCandidate(t *testing.T) {
	fixture := newCheckoutFixture(t)
	stageRecord := "100644 " + testBlobOID + " 0\tignored-staged\x00"
	runner := captureRunner(fixture, stageRecord)
	git := Git{Runner: runner}
	candidate, err := git.BuildCapture(context.Background(), fixture.checkout, fixture.scratch)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.BaseOID != testBaseOID || candidate.ObservedHEAD != testHeadOID || candidate.TreeOID != testTreeOID {
		t.Fatalf("candidate = %+v", candidate)
	}
	assertPrivateIndexBindings(t, runner.calls, fixture.scratch)
}

func TestBuildCaptureRunsGitFromCheckoutRootForRelativeSourceCwd(t *testing.T) {
	fixture := newCheckoutFixture(t)
	nested := filepath.Join(fixture.checkout.CheckoutPath, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.checkout.Source.RelativeCwd = "nested"
	fixture.checkout.Workdir = nested
	runner := captureRunner(fixture, "")
	if _, err := (Git{Runner: runner}).BuildCapture(context.Background(), fixture.checkout, fixture.scratch); err != nil {
		t.Fatal(err)
	}
	for _, command := range runner.calls {
		args := command.Args[3:]
		if slices.Contains(args, "read-tree") || slices.Contains(args, "ls-files") || slices.Contains(args, "add") || slices.Contains(args, "write-tree") || slices.Contains(args, "update-index") {
			if command.Args[2] != fixture.checkout.CheckoutPath {
				t.Fatalf("snapshot command %v ran from %q, want checkout root %q", args, command.Args[2], fixture.checkout.CheckoutPath)
			}
		}
	}
}

func captureRunner(fixture checkoutFixture, stageRecord string) *recordingGitRunner {
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.Workdir, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := fixture.command
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		switch {
		case slices.Equal(args, []string{"ls-files", "--stage", "-z"}):
			return writeCommandOutput(command, stageRecord)
		case slices.Equal(args, []string{"ls-files", "-v", "-z"}):
			return writeCommandOutput(command, "H ordinary.txt\x00")
		case slices.Equal(args, []string{"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"}):
			return writeCommandOutput(command, testHeadOID+"\n")
		case slices.Equal(args, []string{"write-tree"}):
			return writeCommandOutput(command, testTreeOID+"\n")
		case slices.Equal(args, []string{"update-index", "-z", "--index-info"}):
			data, readErr := io.ReadAll(command.Stdin)
			if readErr != nil {
				return readErr
			}
			if string(data) != stageRecord {
				return errors.New("stage-zero overlay was not forwarded exactly")
			}
		}
		return baseHandler(command)
	}
	return runner
}

func assertPrivateIndexBindings(t *testing.T, calls []*exec.Cmd, scratch string) {
	t.Helper()
	for _, command := range calls {
		args := command.Args[3:]
		if slices.Equal(args, []string{"read-tree", "HEAD"}) || slices.Equal(args, []string{"add", "--sparse", "-A"}) || slices.Equal(args, []string{"write-tree"}) {
			if !environmentValue(command.Env, "GIT_INDEX_FILE", scratch) {
				t.Fatalf("private index binding missing from %v", args)
			}
		}
	}
}

func TestExportCapturePreservesBinaryBytesAndNULMetadata(t *testing.T) {
	fixture := newCheckoutFixture(t)
	patch := []byte("diff --git a/x b/x\n@@ -1 +1 @@\n-\x00\xff  \n")
	nameStatus := []byte("M\x00x with space\x00")
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.Workdir, fixture.checkout.AdminDir, testBaseOID)
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if len(args) > 0 && args[0] == "diff" {
			if slices.Contains(args, "--name-status") {
				return writeCommandOutput(command, string(nameStatus))
			}
			_, err := command.Stdout.Write(patch)
			return err
		}
		return fixture.command(command)
	}
	git := Git{Runner: runner}
	export, err := git.ExportCapture(context.Background(), fixture.checkout, CaptureCandidate{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID}, fixture.scratch)
	if err != nil {
		t.Fatal(err)
	}
	assertExportArtifacts(t, export, patch, nameStatus)
	again, err := git.ExportCapture(context.Background(), fixture.checkout, CaptureCandidate{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID}, fixture.scratch)
	if err != nil {
		t.Fatal(err)
	}
	if again.Patch.Path != export.Patch.Path || again.NameStatus.Path != export.NameStatus.Path {
		t.Fatalf("artifact paths changed across replay: first=%+v second=%+v", export, again)
	}
}

func assertExportArtifacts(t *testing.T, export CaptureExport, patch, nameStatus []byte) {
	t.Helper()
	gotPatch, err := os.ReadFile(export.Patch.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPatch, patch) || export.Patch.Length != int64(len(patch)) || export.Patch.SHA256 != digest(patch) {
		t.Fatalf("patch artifact = %+v, bytes = %q", export.Patch, gotPatch)
	}
	if filepath.Base(export.Patch.Path) != "diff.patch" {
		t.Fatalf("patch artifact path = %q, want deterministic diff.patch", export.Patch.Path)
	}
	gotNameStatus, err := os.ReadFile(export.NameStatus.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotNameStatus, nameStatus) || export.NameStatus.SHA256 != digest(nameStatus) {
		t.Fatalf("name-status artifact = %+v, bytes = %q", export.NameStatus, gotNameStatus)
	}
	if filepath.Base(export.NameStatus.Path) != "name-status.z" {
		t.Fatalf("name-status artifact path = %q, want deterministic name-status.z", export.NameStatus.Path)
	}
}

func TestExportCaptureOmitsOversizedArtifactWithoutTruncation(t *testing.T) {
	fixture := newCheckoutFixture(t)
	overLimit := bytes.Repeat([]byte{'x'}, int(ArtifactLimit)+1)
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.Workdir, fixture.checkout.AdminDir, testBaseOID)
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if len(args) > 0 && args[0] == "diff" {
			if slices.Contains(args, "--name-status") {
				return writeCommandOutput(command, "M\x00file\x00")
			}
			_, err := command.Stdout.Write(overLimit)
			return err
		}
		return fixture.command(command)
	}
	git := Git{Runner: runner}
	export, err := git.ExportCapture(context.Background(), fixture.checkout, CaptureCandidate{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID}, fixture.scratch)
	if err != nil {
		t.Fatal(err)
	}
	if !export.Patch.Omitted() || export.Patch.OmittedReason != "limit" || export.Patch.Path != "" || export.Patch.Length != 0 || export.Patch.SHA256 != "" {
		t.Fatalf("oversized patch artifact = %+v", export.Patch)
	}
}

func TestPinBaseUsesCreateOnlyCASAndExpectedObjectFormat(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	runner := newRepositoryFixtureRunner(root, "", "", testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/base", testBaseOID, strings.Repeat("0", 40)}) {
			return nil
		}
		if slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/base"}) {
			return writeCommandOutput(command, string(refInventoryRecord("refs/delegate/base", testBaseOID, "commit", "")))
		}
		return baseHandler(command)
	}
	if err := (Git{Runner: runner}).PinBase(context.Background(), source, "refs/delegate/base"); err != nil {
		t.Fatal(err)
	}
}

type pinBaseRecoveryCase struct {
	name        string
	refOutput   string
	refErr      error
	wantErr     error
	wantSuccess bool
}

func TestPinBaseCreateOnlyCASRecoversMatchingOrClassifiesWinner(t *testing.T) {
	tests := []pinBaseRecoveryCase{
		{name: "matching", refOutput: testBaseOID + "\n", wantSuccess: true},
		{name: "absent"},
		{name: "unreadable 128", refErr: gitTestExitError(128), wantErr: ErrRefUnreadable},
		{name: "conflicting", refOutput: testHeadOID + "\n", wantErr: ErrRefConflict},
		{name: "unreadable exit", refErr: gitTestExitError(2), wantErr: ErrRefUnreadable},
		{name: "unreadable text", refErr: errors.New("fatal: repository state is unclear"), wantErr: ErrRefUnreadable},
		{name: "unreadable data", refOutput: "not-an-oid\n", wantErr: ErrRefUnreadable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runPinBaseRecoveryCase(t, test)
		})
	}
}

func runPinBaseRecoveryCase(t *testing.T, test pinBaseRecoveryCase) {
	t.Helper()
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	runner := newRepositoryFixtureRunner(root, "", "", testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/replay", testBaseOID, strings.Repeat("0", 40)}) {
			return gitTestExitError(1)
		}
		if slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/replay"}) {
			if test.refErr != nil {
				return test.refErr
			}
			if test.refOutput == "" {
				return nil
			}
			return writeCommandOutput(command, string(refInventoryRecord("refs/delegate/replay", removeOneTerminatingNewline(test.refOutput), "commit", "")))
		}
		return baseHandler(command)
	}
	err := (Git{Runner: runner}).PinBase(context.Background(), source, "refs/delegate/replay")
	assertPinBaseRecoveryResult(t, test, err)
	assertSinglePinBaseUpdate(t, runner)
}

func assertPinBaseRecoveryResult(t *testing.T, test pinBaseRecoveryCase, err error) {
	t.Helper()
	if test.wantSuccess {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if test.wantErr == nil {
		if err == nil {
			t.Fatal("operation unexpectedly succeeded")
		}
		return
	}
	if !errors.Is(err, test.wantErr) {
		t.Fatalf("error = %v, want %v", err, test.wantErr)
	}
}

func assertSinglePinBaseUpdate(t *testing.T, runner *recordingGitRunner) {
	t.Helper()
	updateCalls := 0
	for _, command := range runner.calls {
		if slices.Equal(command.Args[3:], []string{"update-ref", "--no-deref", "refs/delegate/replay", testBaseOID, strings.Repeat("0", 40)}) {
			updateCalls++
		}
	}
	if updateCalls != 1 {
		t.Fatalf("update-ref calls = %d, want exactly one", updateCalls)
	}
}

type deleteRefRecoveryCase struct {
	name      string
	refOutput string
	refErr    error
	wantErr   error
}

func TestDeleteRefRecoversOnlyAnAuthoritativelyAbsentRef(t *testing.T) {
	tests := []deleteRefRecoveryCase{
		{name: "absent"},
		{name: "unreadable 128", refErr: gitTestExitError(128), wantErr: ErrRefUnreadable},
		{name: "matching", refOutput: testBaseOID + "\n"},
		{name: "conflicting", refOutput: testHeadOID + "\n", wantErr: ErrRefConflict},
		{name: "unreadable exit", refErr: gitTestExitError(2), wantErr: ErrRefUnreadable},
		{name: "unreadable text", refErr: errors.New("fatal: not found"), wantErr: ErrRefUnreadable},
		{name: "unreadable data", refOutput: "not-an-oid\n", wantErr: ErrRefUnreadable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runDeleteRefRecoveryCase(t, test)
		})
	}
}

func runDeleteRefRecoveryCase(t *testing.T, test deleteRefRecoveryCase) {
	t.Helper()
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	runner := deleteRefRecoveryRunner(root, test)
	err := (Git{Runner: runner}).DeleteRef(context.Background(), source, "refs/delegate/delete", testBaseOID)
	if test.name == "absent" {
		if err != nil {
			t.Fatalf("absent ref recovery: %v", err)
		}
		return
	}
	if test.name == "matching" {
		if err == nil {
			t.Fatal("matching ref unexpectedly succeeded")
		}
		if errors.Is(err, ErrRefConflict) || errors.Is(err, ErrRefUnreadable) {
			t.Fatalf("matching ref classified incorrectly: %v", err)
		}
		var exitErr gitTestExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("matching ref lost original CAS error: %v", err)
		}
		return
	}
	if test.wantErr == nil {
		if err == nil {
			t.Fatal("delete unexpectedly succeeded")
		}
		return
	}
	if !errors.Is(err, test.wantErr) {
		t.Fatalf("error = %v, want %v", err, test.wantErr)
	}
}

func deleteRefRecoveryRunner(root string, test deleteRefRecoveryCase) *recordingGitRunner {
	var inventory []byte
	if test.refOutput != "" {
		inventory = refInventoryRecord("refs/delegate/delete", removeOneTerminatingNewline(test.refOutput), "commit", "")
	}
	runner := newDeleteRefsRunner(root, "refs/delegate/", inventory, gitTestExitError(1))
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/"}) && test.refErr != nil {
			return test.refErr
		}
		return baseHandler(command)
	}
	return runner.recordingGitRunner
}

func TestPinCaptureCreateOnlyCASAcceptsMatchingExistingTreeRef(t *testing.T) {
	fixture := newCheckoutFixture(t)
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/capture", testTreeOID, strings.Repeat("0", 40)}) {
			return gitTestExitError(1)
		}
		if slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/capture"}) {
			return writeCommandOutput(command, string(refInventoryRecord("refs/delegate/capture", testTreeOID, "tree", "")))
		}
		return baseHandler(command)
	}
	candidate := CaptureCandidate{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID, ObjectFormat: "sha1"}
	if err := (Git{Runner: runner}).PinCapture(context.Background(), fixture.checkout, "refs/delegate/capture", candidate); err != nil {
		t.Fatal(err)
	}
}

func TestPinCaptureRejectsObjectTypeMismatchBeforePublication(t *testing.T) {
	fixture := newCheckoutFixture(t)
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"rev-parse", "--verify", "--end-of-options", testTreeOID + "^{tree}"}) {
			return writeCommandOutput(command, testBlobOID+"\n")
		}
		if slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/capture", testTreeOID, strings.Repeat("0", 40)}) {
			t.Fatal("object type mismatch reached ref publication")
		}
		return baseHandler(command)
	}
	candidate := CaptureCandidate{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID, ObjectFormat: "sha1"}
	err := (Git{Runner: runner}).PinCapture(context.Background(), fixture.checkout, "refs/delegate/capture", candidate)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want ErrIdentityMismatch", err)
	}
}

type verifyRemovedCase struct {
	name             string
	removePath       bool
	keepRegistration bool
	wantErr          bool
	wantIdentity     bool
}

func TestVerifyRemovedRequiresPathAndRegistrationAbsence(t *testing.T) {
	tests := []verifyRemovedCase{
		{name: "both absent", removePath: true},
		{name: "path remains", keepRegistration: false, wantErr: true, wantIdentity: true},
		{name: "registration remains", removePath: true, keepRegistration: true, wantErr: true, wantIdentity: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runVerifyRemovedCase(t, test)
		})
	}
}

func runVerifyRemovedCase(t *testing.T, test verifyRemovedCase) {
	t.Helper()
	fixture := newCheckoutFixture(t)
	if test.removePath {
		if err := os.RemoveAll(fixture.checkout.CheckoutPath); err != nil {
			t.Fatal(err)
		}
	}
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"worktree", "list", "--porcelain", "-z"}) && !test.keepRegistration {
			return writeCommandOutput(command, "worktree "+fixture.root+"\x00HEAD "+testBaseOID+"\x00")
		}
		return baseHandler(command)
	}
	err := (Git{Runner: runner}).VerifyRemoved(context.Background(), fixture.checkout)
	if test.wantErr && err == nil {
		t.Fatal("verification unexpectedly succeeded")
	}
	if test.wantIdentity && !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want ErrIdentityMismatch", err)
	}
	if !test.wantErr && err != nil {
		t.Fatal(err)
	}
}

type checkoutFixture struct {
	root     string
	checkout Checkout
	scratch  string
	command  func(*exec.Cmd) error
}

func newCheckoutFixture(t *testing.T) checkoutFixture {
	t.Helper()
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	checkoutPath := filepath.Join(canonicalWorkspaceTestDir(t), "checkout")
	adminDir := filepath.Join(gitDir, "worktrees", "managed")
	if err := os.MkdirAll(adminDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(checkoutPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkoutPath, ".git"), []byte("gitdir: "+adminDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(filepath.Join(checkoutPath, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	checkout := Checkout{Source: source, CheckoutPath: checkoutPath, Workdir: checkoutPath, AdminDir: adminDir, AdminBacklink: adminDir}
	fixture := checkoutFixture{root: root, checkout: checkout, scratch: filepath.Join(canonicalWorkspaceTestDir(t), "scratch")}
	if err := os.Mkdir(fixture.scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := newRepositoryFixtureRunner(root, checkoutPath, adminDir, testBaseOID)
	fixture.command = runner.handler
	return fixture
}

func newRepositoryFixtureRunner(root, checkout, admin, head string) *recordingGitRunner {
	runner := &recordingGitRunner{}
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		directory := command.Args[2]
		if handled, err := repositoryFixtureIdentity(command, args, directory, root, checkout, admin); handled {
			return err
		}
		if handled, err := repositoryFixtureObject(command, args, head); handled {
			return err
		}
		if slices.Equal(args, []string{"symbolic-ref", "-q", "HEAD"}) {
			return gitTestExitError(1)
		}
		if slices.Equal(args, []string{"worktree", "list", "--porcelain", "-z"}) {
			return writeCommandOutput(command, "worktree "+root+"\x00HEAD "+head+"\x00worktree "+checkout+"\x00HEAD "+head+"\x00locked\x00")
		}
		if slices.Equal(args, []string{"worktree", "add", "--detach", "--lock", checkout, head}) {
			return createFixtureCheckout(checkout, admin)
		}
		return nil
	}
	return runner
}

func repositoryFixtureIdentity(command *exec.Cmd, args []string, directory, root, checkout, admin string) (bool, error) {
	switch {
	case slices.Equal(args, []string{"rev-parse", "--show-toplevel"}):
		if directory == checkout {
			return true, writeCommandOutput(command, checkout+"\n")
		}
		return true, writeCommandOutput(command, root+"\n")
	case slices.Equal(args, []string{"rev-parse", "--git-dir"}):
		if directory == checkout {
			return true, writeCommandOutput(command, admin+"\n")
		}
		return true, writeCommandOutput(command, ".git\n")
	case slices.Equal(args, []string{"rev-parse", "--git-common-dir"}):
		if directory == checkout {
			return true, writeCommandOutput(command, filepath.Join(root, ".git")+"\n")
		}
		return true, writeCommandOutput(command, ".git\n")
	case slices.Equal(args, []string{"rev-parse", "--show-object-format=storage"}):
		return true, writeCommandOutput(command, "sha1\n")
	default:
		return false, nil
	}
}

func repositoryFixtureObject(command *exec.Cmd, args []string, head string) (bool, error) {
	if len(args) != 4 || !slices.Equal(args[:3], []string{"rev-parse", "--verify", "--end-of-options"}) {
		return false, nil
	}
	switch {
	case strings.HasSuffix(args[3], "^{commit}"):
		return true, writeCommandOutput(command, objectCommitOutput(args[3], head))
	case strings.HasSuffix(args[3], "^{tree}"):
		return true, writeCommandOutput(command, objectTreeOutput(args[3]))
	default:
		return false, nil
	}
}

func createFixtureCheckout(checkout, admin string) error {
	if err := os.MkdirAll(admin, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(checkout, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(checkout, ".git")+"\n"), 0o600)
}

func objectCommitOutput(expression, head string) string {
	switch expression {
	case "HEAD^{commit}":
		return head + "\n"
	case testBaseOID + "^{commit}":
		return testBaseOID + "\n"
	case testHeadOID + "^{commit}":
		return testHeadOID + "\n"
	default:
		return "\n"
	}
}

func objectTreeOutput(expression string) string {
	if expression == testTreeOID+"^{tree}" {
		return testTreeOID + "\n"
	}
	return "\n"
}

func writeCommandOutput(command *exec.Cmd, data string) error {
	_, err := io.WriteString(command.Stdout, data)
	return err
}

func containsArgs(commands []*exec.Cmd, want []string) bool {
	for _, command := range commands {
		if slices.Equal(command.Args[3:], want) {
			return true
		}
	}
	return false
}

func environmentValue(environment []string, key, scratch string) bool {
	for _, entry := range environment {
		if strings.HasPrefix(entry, key+"="+scratch+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
