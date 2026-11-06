package taskdir

import (
	"os"
	"path/filepath"
	"testing"
)

type storeOwnerFixture struct {
	parent string
	store  *Store
	stores []*Store
	failed bool
}

func newStoreOwnerFixture(t *testing.T) *storeOwnerFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "store-owner-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &storeOwnerFixture{parent: parent}
	t.Cleanup(func() {
		for index := len(fixture.stores) - 1; index >= 0; index-- {
			if closeErr := fixture.stores[index].Close(); closeErr != nil {
				fixture.failed = true
				t.Error(closeErr)
			}
		}
		if fixture.failed {
			t.Logf("retained Store fixture after unresolved closure: %s", fixture.parent)
			return
		}
		if removeErr := os.RemoveAll(fixture.parent); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = InitStore(filepath.Join(parent, "state"))
	if fixture.store != nil {
		fixture.stores = append(fixture.stores, fixture.store)
	}
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *storeOwnerFixture) other(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(f.store.Root)
	if store != nil {
		f.stores = append(f.stores, store)
	}
	if err != nil {
		t.Fatal(err)
	}
	return store
}
