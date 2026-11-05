package infrastructure

import (
	"errors"
	"os"
	"testing"
)

func TestOwnerServerNativeRootReplacementRefusesBeforeHostDispatch(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	f.start()
	request := f.request(t, ownerEnsureOwn)
	connection := f.dial(t)
	retained := f.root + "-retained"
	if err := os.Rename(f.root, retained); err != nil {
		t.Fatal(err)
	}
	// Restore the exact original directory before the fixture can drain and close
	// its borrowed entry. Never remove a nonempty or unidentified replacement.
	t.Cleanup(func() {
		if err := os.Remove(f.root); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.retain = true
			t.Errorf("retain root replacement %s and %s: %v", f.root, retained, err)
			return
		}
		if err := os.Rename(retained, f.root); err != nil {
			f.retain = true
			t.Errorf("retain original root %s: %v", retained, err)
		}
	})
	if err := os.Mkdir(f.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != ownerRefused {
		t.Fatalf("root replacement accepted: %s", reply.Code)
	}
}
