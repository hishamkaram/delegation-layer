package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func worktreeInventoryBytes(paths ...string) []byte {
	var output strings.Builder
	for _, path := range paths {
		output.WriteString("worktree " + path + "\x00HEAD " + testBaseOID + "\x00locked native reason\nwith newline\x00\x00")
	}
	return []byte(output.String())
}

func fixtureRunnerWithInventory(fixture checkoutFixture, paths ...string) *recordingGitRunner {
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	handler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		if slices.Equal(command.Args[3:], []string{"worktree", "list", "--porcelain", "-z"}) {
			return writeCommandOutput(command, string(worktreeInventoryBytes(paths...)))
		}
		return handler(command)
	}
	return runner
}

func TestInspectCleanupIgnoresUnrelatedRegularFileRegistration(t *testing.T) {
	fixture := newCheckoutFixture(t)
	unrelated := filepath.Join(fixture.root, "unrelated-worktree")
	if err := os.WriteFile(unrelated, []byte("unrelated path became a regular file"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := fixtureRunnerWithInventory(fixture, unrelated, fixture.checkout.CheckoutPath)
	got, err := (Git{Runner: runner}).InspectCleanup(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if err != nil || got.CheckoutPath != fixture.checkout.CheckoutPath {
		t.Fatalf("unrelated filesystem type invalidated the owned target: %+v %v", got, err)
	}
}

func worktreeParentAlias(t *testing.T, target string) string {
	t.Helper()
	parent := filepath.Join(canonicalWorkspaceTestDir(t), "parent-alias")
	if err := os.Symlink(filepath.Dir(target), parent); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, filepath.Base(target))
}

func TestWorktreeInventoryPreservesAliasesAndDuplicateTargetRows(t *testing.T) {
	target := canonicalWorkspaceTestDir(t)
	alias := worktreeParentAlias(t, target)
	secondAlias := worktreeParentAlias(t, target)
	cases := []struct {
		name      string
		paths     []string
		ambiguous bool
	}{
		{"single-alias", []string{alias}, false},
		{"repeated-raw-target", []string{target, target}, true},
		{"canonical-and-alias", []string{target, alias}, true},
		{"alias-and-canonical", []string{alias, target}, true},
		{"two-aliases", []string{alias, secondAlias}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			entries, err := parseWorktreeRegistrations(worktreeInventoryBytes(test.paths...))
			if err != nil || len(entries) != len(test.paths) {
				t.Fatalf("inventory discarded native rows: %+v %v", entries, err)
			}
			registration, found, err := matchWorktreeRegistration(entries, target)
			if test.ambiguous {
				if !errors.Is(err, ErrIdentityMismatch) {
					t.Fatalf("duplicate aliases selected an arbitrary registration: %v", err)
				}
			} else if err != nil || !found || !registration.locked {
				t.Fatalf("single valid alias did not resolve: %+v %t %v", registration, found, err)
			}
		})
	}
}

func TestDuplicateTargetRegistrationCannotAuthorizeUnlockOrRemoval(t *testing.T) {
	fixture := newCheckoutFixture(t)
	alias := worktreeParentAlias(t, fixture.checkout.CheckoutPath)
	runner := fixtureRunnerWithInventory(fixture, fixture.checkout.CheckoutPath, alias)
	if err := (Git{Runner: runner}).Remove(context.Background(), fixture.checkout); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("ambiguous target removal error=%v", err)
	}
	for _, call := range runner.calls {
		args := call.Args[3:]
		if len(args) >= 2 && args[0] == "worktree" && (args[1] == "unlock" || args[1] == "remove") {
			t.Fatalf("ambiguous registration authorized destructive Git command: %v", args)
		}
	}
}

func TestVerifyRemovedDistinguishesTargetAliasFromUnrelatedFile(t *testing.T) {
	fixture := newCheckoutFixture(t)
	alias := worktreeParentAlias(t, fixture.checkout.CheckoutPath)
	if err := os.RemoveAll(fixture.checkout.CheckoutPath); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(fixture.root, "unrelated-file")
	if err := os.WriteFile(unrelated, []byte("not a worktree directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := fixtureRunnerWithInventory(fixture, unrelated, alias)
	if err := (Git{Runner: runner}).VerifyRemoved(context.Background(), fixture.checkout); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("stale alias registration became proof of removal: %v", err)
	}
	runner = fixtureRunnerWithInventory(fixture, unrelated)
	if err := (Git{Runner: runner}).VerifyRemoved(context.Background(), fixture.checkout); err != nil {
		t.Fatalf("unrelated regular file blocked exact absence proof: %v", err)
	}
}

func TestWorktreeInventoryUnresolvedAliasCannotProveTargetIdentity(t *testing.T) {
	target := canonicalWorkspaceTestDir(t)
	loop := filepath.Join(canonicalWorkspaceTestDir(t), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	entries, err := parseWorktreeRegistrations(worktreeInventoryBytes(target, loop))
	if err != nil {
		t.Fatalf("lexical parser performed unrelated filesystem resolution: %v", err)
	}
	if _, _, err = matchWorktreeRegistration(entries, target); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("unresolved possible alias became unique target proof: %v", err)
	}
}

func TestWorktreeInventoryConsumesMalformedSuffixAfterValidTarget(t *testing.T) {
	target := canonicalWorkspaceTestDir(t)
	data := append(worktreeInventoryBytes(target), []byte("worktree /truncated-without-nul")...)
	if _, err := parseWorktreeRegistrations(data); err == nil {
		t.Fatal("parser accepted a valid prefix without consuming the complete inventory")
	}
}
