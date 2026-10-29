package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWithExistingBootstrapLockContextNeverProvisions: the existing-only
// bootstrap boundary refuses an absent root, including under a canceled
// context, without creating any ancestor, and enters a published lock.
func TestWithExistingBootstrapLockContextNeverProvisions(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, ctx := range map[string]context.Context{"live": t.Context(), "canceled": canceled} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(canonicalTemp(t), "state")
			called := false
			err := WithExistingBootstrapLockContext(ctx, root, func(context.Context) error {
				called = true
				return nil
			})
			if called || !errors.Is(err, ErrPrivateSupervisorNotInitialized) {
				t.Fatalf("absent root called=%t err=%v", called, err)
			}
			if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("absent root was created: %v", statErr)
			}
		})
	}
	fixture := newPrivateSupervisorTestFixture(t)
	bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	called := false
	if err := WithExistingBootstrapLockContext(t.Context(), fixture.stateRoot, func(context.Context) error {
		called = true
		return nil
	}); err != nil || !called {
		t.Fatalf("published bootstrap lock called=%t err=%v", called, err)
	}
}
