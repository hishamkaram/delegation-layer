package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const absenceTestRef = "refs/delegate/root/workspaces/one/capture"

var errTestBrokenPipe = &os.PathError{Op: "write", Path: "|0", Err: errors.New("broken pipe")}

type refAbsenceBatchCase struct {
	name            string
	stdout          string
	stderr          string
	runErr          error
	wantProven      bool
	wantUnsupported bool
	wantErr         []error
}

func TestClassifyRefAbsenceBatch(t *testing.T) {
	unknown := "fatal: unknown command: symref-verify " + absenceTestRef + "\n"
	tests := []refAbsenceBatchCase{
		{name: "proven", stdout: refAbsenceProvenStdout, wantProven: true},
		{name: "unsupported", stdout: "start: ok\n", stderr: unknown, runErr: gitTestExitError(128), wantUnsupported: true},
		{name: "success stderr", stdout: refAbsenceProvenStdout, stderr: "warning: x\n", wantErr: []error{ErrRefUnreadable, errUnexpectedRefAbsenceOutput}},
		{name: "success stdout wrong", stdout: "start: ok\nprepare: ok\n", wantErr: []error{ErrRefUnreadable, errUnexpectedRefAbsenceOutput}},
		{name: "existing ref refused", stdout: "start: ok\n", stderr: "fatal: cannot lock ref\n", runErr: gitTestExitError(128), wantErr: []error{ErrRefUnreadable}},
		{name: "unknown with extra text", stdout: "start: ok\n", stderr: unknown + "hint: x\n", runErr: gitTestExitError(128), wantErr: []error{ErrRefUnreadable}},
		{name: "unknown different ref", stdout: "start: ok\n", stderr: "fatal: unknown command: symref-verify " + absenceTestRef + "-other\n", runErr: gitTestExitError(128), wantErr: []error{ErrRefUnreadable}},
		{name: "unknown localized", stdout: "start: ok\n", stderr: "fatal: unbekannter Befehl: symref-verify " + absenceTestRef + "\n", runErr: gitTestExitError(128), wantErr: []error{ErrRefUnreadable}},
		{name: "unknown stdout empty", stderr: unknown, runErr: gitTestExitError(128), wantErr: []error{ErrRefUnreadable}},
		{name: "unknown other exit", stdout: "start: ok\n", stderr: unknown, runErr: gitTestExitError(129), wantErr: []error{ErrRefUnreadable, gitTestExitError(129)}},
		{name: "unknown bounded output", stdout: "start: ok\n", stderr: unknown, runErr: errors.Join(gitTestExitError(128), errors.New("git command output exceeded the scalar bound")), wantErr: []error{ErrRefUnreadable}},
		{name: "unknown joined close", stdout: "start: ok\n", stderr: unknown, runErr: errors.Join(gitTestExitError(128), errTestJoinedStreamClose), wantErr: []error{ErrRefUnreadable, errTestJoinedStreamClose}},
		{name: "unknown context", stdout: "start: ok\n", stderr: unknown, runErr: errors.Join(gitTestExitError(128), context.Canceled), wantErr: []error{ErrRefUnreadable, context.Canceled}},
		{name: "unknown EPIPE", stdout: "start: ok\n", stderr: unknown, runErr: errors.Join(gitTestExitError(128), errTestBrokenPipe), wantErr: []error{ErrRefUnreadable, errTestBrokenPipe}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyRefAbsenceBatch([]byte(test.stdout), []byte(test.stderr), test.runErr, absenceTestRef)
			switch {
			case test.wantProven:
				if err != nil {
					t.Fatalf("proven batch: %v", err)
				}
			case test.wantUnsupported:
				if !errors.Is(err, errRefAbsenceVerificationUnsupported) || errors.Is(err, ErrRefUnreadable) {
					t.Fatalf("error = %v, want only unsupported", err)
				}
			default:
				if errors.Is(err, errRefAbsenceVerificationUnsupported) {
					t.Fatalf("near-miss selected the legacy fallback: %v", err)
				}
				assertErrorsIs(t, err, test.wantErr)
			}
		})
	}
}

func TestClassifyRefStorageConfig(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		stderr string
		runErr error
		accept bool
	}{
		{name: "unset", runErr: gitTestExitError(1), accept: true},
		{name: "files", stdout: "files\n", accept: true},
		{name: "reftable", stdout: "reftable\n"},
		{name: "ambiguous", stdout: "files\nreftable\n"},
		{name: "repeated", stdout: "files\nfiles\n"},
		{name: "payload", stdout: "files://elsewhere\n"},
		{name: "case", stdout: "Files\n"},
		{name: "unset with stderr", stderr: "warning: x\n", runErr: gitTestExitError(1)},
		{name: "unset with joined close", runErr: errors.Join(gitTestExitError(1), errTestJoinedStreamClose)},
		{name: "fatal", runErr: gitTestExitError(128)},
		{name: "files with stderr", stdout: "files\n", stderr: "warning: x\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyRefStorageConfig([]byte(test.stdout), []byte(test.stderr), test.runErr)
			if test.accept {
				if err != nil {
					t.Fatalf("files storage refused: %v", err)
				}
				return
			}
			assertErrorsIs(t, err, []error{ErrRefUnreadable, errLegacyRefStorageUnprovable})
		})
	}
}

type deleteRefsAbsenceCase struct {
	name        string
	inventory   []byte
	expected    []string
	hidden      []string
	legacy      bool
	refStorage  string
	environment string
	files       map[string]string
	symlinks    map[string]string
	wantErr     []error
	wantVerify  []int
	wantDeletes int
	wantConfig  int
}

func TestDeleteRefsProvesInventoryMissingRefsAbsent(t *testing.T) {
	const prefix = "refs/delegate/root/workspaces/one/"
	listed := refInventoryRecord(prefix+"listed", testTreeOID, "tree", "")
	all := []string{prefix + "listed", prefix + "missing-a", prefix + "missing-b"}
	tests := []deleteRefsAbsenceCase{
		{name: "all listed", inventory: listed, expected: []string{prefix + "listed"}, wantDeletes: 1},
		{name: "all missing", expected: all[1:], wantVerify: []int{1, 1}},
		{name: "mixed", inventory: listed, expected: all, wantVerify: []int{1, 1}, wantDeletes: 1},
		{name: "conflicting listed direct", inventory: refInventoryRecord(prefix+"listed", testHeadOID, "tree", ""), expected: all, wantErr: []error{ErrRefConflict}},
		{name: "live symbolic listed", inventory: refInventoryRecord(prefix+"listed", testTreeOID, "tree", "refs/heads/main"), expected: all, wantErr: []error{ErrRefUnreadable}},
		{name: "dangling first", inventory: listed, expected: all, hidden: all[1:2], wantErr: []error{ErrRefUnreadable}, wantVerify: []int{1}},
		{name: "dangling later", inventory: listed, expected: all, hidden: all[2:], wantErr: []error{ErrRefUnreadable}, wantVerify: []int{1, 1}},
		{name: "legacy absent", inventory: listed, expected: all, legacy: true, wantVerify: []int{1}, wantConfig: 1, wantDeletes: 1},
		{name: "legacy explicit files", expected: all[1:], legacy: true, refStorage: "files\n", wantVerify: []int{1}, wantConfig: 1},
		{
			name: "legacy packed control", inventory: listed, expected: all, legacy: true,
			files:      map[string]string{"packed-refs": testTreeOID + " " + prefix + "listed\n"},
			wantVerify: []int{1}, wantConfig: 1, wantDeletes: 1,
		},
		{
			name: "legacy dangling later", inventory: listed, expected: all, legacy: true,
			files:      map[string]string{prefix + "missing-b": "ref: refs/heads/deleted\n"},
			wantErr:    []error{ErrRefUnreadable, errHiddenOwnedRef},
			wantVerify: []int{1}, wantConfig: 1,
		},
		{
			name: "legacy directory at name", expected: all[1:], legacy: true,
			files:      map[string]string{prefix + "missing-a/child": ""},
			wantErr:    []error{ErrRefUnreadable, errHiddenOwnedRef},
			wantVerify: []int{1}, wantConfig: 1,
		},
		{
			name: "legacy symlink parent", expected: all[1:], legacy: true,
			files:      map[string]string{"elsewhere/keep": ""},
			symlinks:   map[string]string{"refs/delegate": "../elsewhere"},
			wantErr:    []error{ErrRefUnreadable},
			wantVerify: []int{1}, wantConfig: 1,
		},
		{
			name: "legacy file parent", expected: all[1:], legacy: true,
			files:      map[string]string{"refs/delegate/root": ""},
			wantErr:    []error{ErrRefUnreadable},
			wantVerify: []int{1}, wantConfig: 1,
		},
		{name: "legacy reftable", expected: all[1:], legacy: true, refStorage: "reftable\n", wantErr: []error{ErrRefUnreadable, errLegacyRefStorageUnprovable}, wantVerify: []int{1}, wantConfig: 1},
		{name: "legacy ambiguous", expected: all[1:], legacy: true, refStorage: "files\nreftable\n", wantErr: []error{ErrRefUnreadable, errLegacyRefStorageUnprovable}, wantVerify: []int{1}, wantConfig: 1},
		{name: "legacy backend override", expected: all[1:], legacy: true, environment: "GIT_REFERENCE_BACKEND", wantErr: []error{ErrRefUnreadable, errLegacyRefStorageUnprovable}, wantVerify: []int{1}},
		{name: "legacy storage format override", expected: all[1:], legacy: true, environment: "GIT_REF_STORAGE_FORMAT", wantErr: []error{ErrRefUnreadable, errLegacyRefStorageUnprovable}, wantVerify: []int{1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runDeleteRefsAbsenceCase(t, prefix, test) })
	}
}

func runDeleteRefsAbsenceCase(t *testing.T, prefix string, test deleteRefsAbsenceCase) {
	t.Helper()
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCommonDirFixture(t, gitDir, test.files, test.symlinks)
	if test.environment != "" {
		t.Setenv(test.environment, "files")
	}
	runner := newDeleteRefsRunner(root, prefix, test.inventory, nil)
	runner.legacy = test.legacy
	runner.refStorage = test.refStorage
	runner.hidden = make(map[string]bool)
	for _, name := range test.hidden {
		runner.hidden[name] = true
	}
	expected := make([]ForgetRef, 0, len(test.expected))
	for _, ref := range test.expected {
		expected = append(expected, ForgetRef{Ref: ref, ExpectedOID: testTreeOID})
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	err := (Git{Runner: runner}).DeleteRefs(context.Background(), source, prefix, expected)
	if test.wantErr == nil && err != nil {
		t.Fatalf("delete refs: %v", err)
	}
	assertErrorsIs(t, err, test.wantErr)
	assertAbsenceBatchNames(t, runner.verifyInputs, test.wantVerify)
	if runner.configReads != test.wantConfig {
		t.Fatalf("ref storage config reads = %d, want %d", runner.configReads, test.wantConfig)
	}
	deletes := 0
	for _, transaction := range runner.updateInputs {
		deletes += bytes.Count(transaction, []byte("delete "))
	}
	if len(runner.updateInputs) > 1 || deletes != test.wantDeletes {
		t.Fatalf("mutating transactions = %d with %d deletes, want at most one with %d", len(runner.updateInputs), deletes, test.wantDeletes)
	}
}

// writeCommonDirFixture creates files and symlinks relative to the common
// directory; a path ending in "/child" leaves a directory at its parent.
func writeCommonDirFixture(t *testing.T, gitDir string, files, symlinks map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(gitDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range symlinks {
		path := filepath.Join(gitDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
}

// A second batch never falls back: support cannot change between batches of
// one deletion, so its unknown-command refusal is unreadable.
func TestDeleteRefsSecondBatchUnsupportedIsUnreadable(t *testing.T) {
	const prefix = "refs/delegate/root/workspaces/one/"
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := newDeleteRefsRunner(root, prefix, nil, nil)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		if len(runner.verifyInputs) == 1 && command.Args[3] == "update-ref" {
			runner.legacy = true
		}
		return baseHandler(command)
	}
	expected := []ForgetRef{{Ref: prefix + "a", ExpectedOID: testTreeOID}, {Ref: prefix + "b", ExpectedOID: testTreeOID}}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	err := (Git{Runner: runner}).DeleteRefs(context.Background(), source, prefix, expected)
	assertErrorsIs(t, err, []error{ErrRefUnreadable, errRefAbsenceVerificationUnsupported})
	if len(runner.verifyInputs) != 2 || runner.configReads != 0 || len(runner.updateInputs) != 0 {
		t.Fatalf("verify=%d config=%d mutations=%d, want 2/0/0", len(runner.verifyInputs), runner.configReads, len(runner.updateInputs))
	}
}

func TestLooseRefsAbsentKeepsContextCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := looseRefsAbsent(ctx, canonicalWorkspaceTestDir(t), []string{absenceTestRef})
	assertErrorsIs(t, err, []error{ErrRefUnreadable, context.Canceled})
}

func TestInheritedRefBackendOverrideMatchesExactNames(t *testing.T) {
	if _, set := inheritedRefBackendOverride([]string{"GIT_REFERENCE_BACKEND_X=1", "XGIT_REF_STORAGE_FORMAT=1", "PATH=/bin"}); set {
		t.Fatal("unrelated variable selected a backend override")
	}
	if variable, set := inheritedRefBackendOverride([]string{"GIT_REFERENCE_BACKEND="}); !set || variable != "GIT_REFERENCE_BACKEND" {
		t.Fatalf("empty inherited override = %q, %v; want refused", variable, set)
	}
}

// assertAbsenceBatchNames checks each verify-only batch's symref-verify count.
func assertAbsenceBatchNames(t *testing.T, batches [][]byte, want []int) {
	t.Helper()
	if len(batches) != len(want) {
		t.Fatalf("absence verification batches = %d, want %d", len(batches), len(want))
	}
	for index, names := range want {
		if got := bytes.Count(batches[index], []byte("symref-verify ")); got != names {
			t.Fatalf("absence verification batch %d names = %d, want %d", index, got, names)
		}
	}
}

func assertErrorsIs(t *testing.T, err error, wants []error) {
	t.Helper()
	if len(wants) != 0 && err == nil {
		t.Fatal("operation unexpectedly succeeded")
	}
	for _, want := range wants {
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	}
}
