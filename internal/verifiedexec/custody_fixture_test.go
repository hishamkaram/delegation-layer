package verifiedexec

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	proofReceiptEnv = "DELEGATE_CUSTODY_PROOF_RECEIPT_DIR"
	proofScratchEnv = "DELEGATE_CUSTODY_PROOF_SCRATCH"
)

// scratch returns a test-owned directory below the operator's observed native
// proof directory when given, else below the native temporary root. Views are
// sealed read-only, so removal unseals them first.
func scratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv(proofScratchEnv), "custody-")
	if err != nil {
		t.Fatalf("setup blocked: scratch directory: %v", err)
	}
	t.Cleanup(func() {
		if unsealErr := unsealTemporaryDirectory(dir); unsealErr != nil {
			t.Errorf("unseal scratch: %v", unsealErr)
		}
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			t.Errorf("remove scratch: %v", removeErr)
		}
	})
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("canonical scratch: %v", err)
	}
	if err = os.Chmod(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	return canonical
}
