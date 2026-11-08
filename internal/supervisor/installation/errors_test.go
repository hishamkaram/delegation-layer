package installation

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestRecoveryCandidatePreservesJoinedBundleFaults(t *testing.T) {
	original := errors.Join(pueue.ErrConfiguration, os.ErrNotExist, os.ErrPermission)
	classified := classifyUnavailableBundledPair(original)
	var unavailable *unavailableBundledPairError
	if errors.As(classified, &unavailable) || !errors.Is(classified, os.ErrPermission) || !errors.Is(classified, os.ErrNotExist) {
		t.Fatalf("joined bundle fault became unavailable: %v", classified)
	}
	missing := classifyUnavailableBundledPair(errors.Join(pueue.ErrConfiguration, os.ErrNotExist))
	if !errors.As(missing, &unavailable) || !errors.Is(missing, os.ErrNotExist) || !errors.Is(missing, pueue.ErrConfiguration) {
		t.Fatalf("genuine bundle absence lost diagnostic causes: %v", missing)
	}
}
