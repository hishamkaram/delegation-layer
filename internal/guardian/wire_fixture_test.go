package guardian

import (
	"errors"
	"io"
	"os"
	"testing"
)

func cleanupLifetimeTestResource(t *testing.T, resource io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := resource.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close test resource: %v", err)
		}
	})
}
