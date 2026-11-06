package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type helperAdmissionFixture struct {
	store       *taskdir.Store
	owners      []*taskdir.LifetimeOwnerLease
	controls    []*taskdir.ControlDir
	otherStores []*taskdir.Store
}

func newHelperAdmissionFixture(t *testing.T) *helperAdmissionFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "admission-helper-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &helperAdmissionFixture{}
	t.Cleanup(func() {
		if closeErr := fixture.closeOwned(); closeErr != nil {
			t.Error(closeErr)
			t.Logf("retained unresolved admission helper fixture: %s", parent)
			return
		}
		if removeErr := os.RemoveAll(parent); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = taskdir.InitStore(filepath.Join(parent, "state"))
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *helperAdmissionFixture) closeOwned() error {
	var resultErr error
	for index := len(f.owners) - 1; index >= 0; index-- {
		resultErr = errors.Join(resultErr, f.owners[index].Release())
	}
	if resultErr != nil {
		return resultErr
	}
	for index := len(f.controls) - 1; index >= 0; index-- {
		resultErr = errors.Join(resultErr, f.controls[index].Close())
	}
	if resultErr != nil {
		return resultErr
	}
	for _, store := range f.otherStores {
		resultErr = errors.Join(resultErr, store.Close())
	}
	if resultErr != nil {
		return resultErr
	}
	if f.store != nil {
		return f.store.Close()
	}
	return nil
}
