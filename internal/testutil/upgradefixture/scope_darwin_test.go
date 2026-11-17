//go:build darwin

package main

import (
	"errors"
	"testing"
)

// This exercises Darwin's actual owner-bound census. A policy mock cannot
// detect omitting BindOwnerProcess, and another kernel does not prove this.
func TestDarwinFixtureScopeBindsNativeOwnerBeforeRegistration(t *testing.T) {
	store, op := fixtureInspectionOperation(t)
	owner, err := op.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	native, _, err := createFixtureScope(t.Context(), store.RootID, op.Request().IdentityID(), op)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(native.DestroyEmpty(), native.Release()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	empty, err := native.EmptyContext(t.Context(), nil)
	if err != nil || !empty {
		t.Fatalf("actual fixture scope owner association missing: empty=%t err=%v", empty, err)
	}
}
