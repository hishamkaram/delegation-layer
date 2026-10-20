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

	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
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
	if want := wantIndexIdentity("H "+stageRecord, testVisibleIndex); candidate.IndexSHA256 != want {
		t.Fatalf("index identity = %s, want framed inventory and visible-entry digests %s", candidate.IndexSHA256, want)
	}
	assertPrivateIndexBindings(t, runner.calls, fixture.scratch)
}

// Both index observations read one frozen copy of the real index, opened once,
// never the live index or the private index that capture mutates.
func TestBuildCaptureObservesOneFrozenIndexImage(t *testing.T) {
	fixture := newCheckoutFixture(t)
	live := filepath.Join(fixture.checkout.AdminDir, "index")
	if err := os.WriteFile(live, []byte("opaque index image v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := captureRunner(fixture, "")
	inner := runner.handler
	var observed []string
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"ls-files", "--stage", "-v", "-z"}) || slices.Equal(args, visibleIndexArgs) {
			image := indexOverride(command.Env)
			data, err := os.ReadFile(image)
			if err != nil || string(data) != "opaque index image v1" {
				return errors.Join(errors.New("index observation did not read the frozen image"), err)
			}
			observed = append(observed, image)
			// The live index is replaced between observations; the frozen
			// image must not follow it.
			if err = os.WriteFile(live, []byte("opaque index image v2"), 0o600); err != nil {
				return err
			}
		}
		return inner(command)
	}
	if _, err := (Git{Runner: runner}).BuildCapture(context.Background(), fixture.checkout, fixture.scratch); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] != observed[1] || filepath.Dir(observed[0]) != fixture.scratch || observed[0] == live {
		t.Fatalf("index observations read %q, want one frozen image in the capture scratch", observed)
	}
	if _, err := os.Stat(observed[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("frozen index image was not removed after capture: %v", err)
	}
}

func TestBuildCaptureRefusesUnreadableRealIndex(t *testing.T) {
	fixture := newCheckoutFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.checkout.AdminDir, "index"), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate, err := (Git{Runner: captureRunner(fixture, "")}).BuildCapture(context.Background(), fixture.checkout, fixture.scratch)
	if err == nil || candidate.IndexSHA256 != "" {
		t.Fatalf("capture of a non-regular real index = %+v, %v; want an explicit failure", candidate, err)
	}
}

func indexOverride(environment []string) string {
	value := ""
	for _, entry := range environment {
		if path, found := strings.CutPrefix(entry, "GIT_INDEX_FILE="); found {
			value = path
		}
	}
	return value
}

// A Git that cannot hide intent-to-add entries must fail the capture, never
// fall back to an inventory that would read an intent change as unchanged.
func TestBuildCaptureFailsWhenVisibleIndexEntriesAreUnavailable(t *testing.T) {
	fixture := newCheckoutFixture(t)
	runner := captureRunner(fixture, "")
	inner := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		if slices.Equal(command.Args[3:], visibleIndexArgs) {
			return errors.New("error: unknown option `ita-invisible-in-index'")
		}
		return inner(command)
	}
	candidate, err := (Git{Runner: runner}).BuildCapture(context.Background(), fixture.checkout, fixture.scratch)
	if err == nil || candidate.IndexSHA256 != "" {
		t.Fatalf("capture without visible index entries = %+v, %v; want an explicit failure", candidate, err)
	}
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

const (
	testEmptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	testVisibleIndex = ":000000 100644 0000000000000000000000000000000000000000 " + testBlobOID + " A\x00ignored-staged\x00"
)

var visibleIndexArgs = []string{"--no-replace-objects", "diff-index", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--ita-invisible-in-index", "--end-of-options", testEmptyTreeOID}

// wantIndexIdentity frames the inventory and visible-entry stream digests as
// the capture's semantic index identity.
func wantIndexIdentity(inventory, visible string) string {
	return digest([]byte(digest([]byte(inventory)) + "\n" + digest([]byte(visible)) + "\n"))
}

func captureRunner(fixture checkoutFixture, stageRecord string) *recordingGitRunner {
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.Workdir, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := fixture.command
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		switch {
		case slices.Equal(args, []string{"ls-files", "--stage", "-v", "-z"}):
			if stageRecord == "" {
				return nil
			}
			return writeCommandOutput(command, "H "+stageRecord)
		case slices.Equal(args, []string{"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"}):
			return writeCommandOutput(command, testHeadOID+"\n")
		case slices.Equal(args, []string{"write-tree"}):
			return writeCommandOutput(command, testTreeOID+"\n")
		case slices.Equal(args, []string{"hash-object", "-t", "tree", "--stdin"}):
			return writeCommandOutput(command, testEmptyTreeOID+"\n")
		case slices.Equal(args, visibleIndexArgs):
			return writeCommandOutput(command, testVisibleIndex)
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

func TestIndexInventoryDigestsTagsAndRoutesStageZeroAndAbsentSkipped(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "present"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := func(stage, path string) string { return "100644 " + testBlobOID + " " + stage + "\t" + path + "\x00" }
	inventory := "h " + entry("0", "assumed") + "S " + entry("0", "absent skipped") + "s " + entry("0", "present") + "M " + entry("1", "conflict") + "m " + entry("3", "conflict")
	var overlay, skip bytes.Buffer
	sink := indexInventorySink{overlay: &overlay, skip: &skip, workdir: workdir, objectFormat: "sha1"}
	got, err := copyIndexInventory(strings.NewReader(inventory), sink)
	if err != nil {
		t.Fatal(err)
	}
	if got != digest([]byte(inventory)) {
		t.Fatalf("digest %s does not cover every tagged record", got)
	}
	if want := entry("0", "assumed") + entry("0", "absent skipped") + entry("0", "present"); overlay.String() != want {
		t.Fatalf("overlay = %q, want stage-zero entries without tags %q", overlay.String(), want)
	}
	if skip.String() != "absent skipped\x00" {
		t.Fatalf("skipped paths = %q, want only the absent skip-worktree path", skip.String())
	}
	for _, malformed := range []string{"X " + entry("0", "unknown tag"), "H " + entry("2", "cached conflict"), "M " + entry("0", "unmerged stage zero"), "H" + entry("0", "untagged")} {
		if _, err = copyIndexInventory(strings.NewReader(malformed), indexInventorySink{overlay: io.Discard, skip: io.Discard, workdir: workdir, objectFormat: "sha1"}); err == nil {
			t.Fatalf("malformed inventory %q was accepted", malformed)
		}
	}
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
	created := false
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/base", testBaseOID, strings.Repeat("0", 40)}) {
			created = true
			return nil
		}
		if slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/base"}) && created {
			return writeCommandOutput(command, string(refInventoryRecord("refs/delegate/base", testBaseOID, "commit", "")))
		}
		return baseHandler(command)
	}
	if err := (Git{Runner: runner}).PinBase(context.Background(), source, "refs/delegate/base"); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("absent base ref was not created")
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
	// The pre-CAS observation sees an absent ref; the ambiguous CAS failure
	// is then followed by the recorded winner or inspection fault.
	attempted := false
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/replay", testBaseOID, strings.Repeat("0", 40)}) {
			attempted = true
			return gitTestExitError(1)
		}
		if slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/replay"}) && attempted {
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
	attempted := false
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/capture", testTreeOID, strings.Repeat("0", 40)}) {
			attempted = true
			return gitTestExitError(1)
		}
		if slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/capture"}) && attempted {
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

type pinTurnBaselineCase struct {
	name        string
	mutate      func(*TurnBaseline)
	missing     bool
	winner      string
	want        error
	wantRefusal bool
}

// A baseline pin needs its recorded tree and the source base, never the
// baseline HEAD commit, which a rewritten history may have pruned.
func TestPinTurnBaselineNeedsTreeNotPrunedHEAD(t *testing.T) {
	tests := []pinTurnBaselineCase{
		{name: "pruned HEAD"},
		{name: "matching winner", winner: testTreeOID},
		{name: "conflicting winner", winner: testBlobOID, want: ErrRefConflict},
		{name: "missing tree", missing: true, wantRefusal: true},
		{name: "foreign ref", mutate: func(baseline *TurnBaseline) { baseline.Ref = "refs/heads/baseline" }, wantRefusal: true},
		{name: "foreign base", mutate: func(baseline *TurnBaseline) { baseline.BaseOID = testHeadOID }, want: ErrIdentityMismatch},
		{name: "malformed tree", mutate: func(baseline *TurnBaseline) { baseline.TreeOID = "tree" }, wantRefusal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runPinTurnBaselineCase(t, test) })
	}
}

func runPinTurnBaselineCase(t *testing.T, test pinTurnBaselineCase) {
	t.Helper()
	fixture := newCheckoutFixture(t)
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	published := false
	runner.handler = pinTurnBaselineHandler(test, &published, runner.handler)
	baseline := TurnBaseline{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID, ObjectFormat: "sha1", Ref: "refs/delegate/baseline"}
	if test.mutate != nil {
		test.mutate(&baseline)
	}
	err := (Git{Runner: runner}).PinTurnBaseline(context.Background(), fixture.checkout, baseline)
	switch {
	case test.want != nil:
		if !errors.Is(err, test.want) {
			t.Fatalf("error = %v, want %v", err, test.want)
		}
	case test.wantRefusal:
		if err == nil || published {
			t.Fatalf("invalid baseline pin was published: published=%v, %v", published, err)
		}
	case err != nil || !published:
		t.Fatalf("valid baseline pin: published=%v, %v", published, err)
	}
}

// pinTurnBaselineHandler models a repository whose baseline HEAD commit is
// gone; consulting it fails the case.
func pinTurnBaselineHandler(test pinTurnBaselineCase, published *bool, baseHandler func(*exec.Cmd) error) func(*exec.Cmd) error {
	return func(command *exec.Cmd) error {
		args := command.Args[3:]
		switch {
		case slices.Contains(args, testHeadOID+"^{commit}"):
			return errors.New("baseline HEAD commit was consulted")
		case test.missing && slices.Equal(args, []string{"rev-parse", "--verify", "--end-of-options", testTreeOID + "^{tree}"}):
			return gitTestExitError(128)
		case slices.Equal(args, []string{"update-ref", "--no-deref", "refs/delegate/baseline", testTreeOID, strings.Repeat("0", 40)}):
			*published = true
			if test.winner != "" {
				return gitTestExitError(1)
			}
			return nil
		case slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, "refs/delegate/baseline"}):
			if !*published {
				return nil
			}
			target := test.winner
			if target == "" {
				target = testTreeOID
			}
			return writeCommandOutput(command, string(refInventoryRecord("refs/delegate/baseline", target, "tree", "")))
		}
		return baseHandler(command)
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
		// The fixture repository holds no symbolic owned ref, so an exact
		// owned-ref query is native's clean not-symbolic exit.
		if isOwnedSymbolicRefQuery(args) {
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
		return true, writeCommandOutput(command, gitDirectoryAnswer(directory, root))
	case slices.Equal(args, []string{"rev-parse", "--git-common-dir"}):
		if directory == checkout {
			return true, writeCommandOutput(command, filepath.Join(root, ".git")+"\n")
		}
		return true, writeCommandOutput(command, gitDirectoryAnswer(directory, root))
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

// isOwnedSymbolicRefQuery matches the exact single-name owned-ref observation.
func isOwnedSymbolicRefQuery(args []string) bool {
	return len(args) == 4 && slices.Equal(args[:3], []string{"symbolic-ref", "--quiet", "--no-recurse"}) && contract.ValidateOwnedRef(args[3]) == nil
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
