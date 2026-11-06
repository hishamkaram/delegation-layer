package supervisor

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestInfrastructureTargetPinRefusesOtherActualStore(t *testing.T) {
	first, second := newHelperAdmissionFixture(t), newHelperAdmissionFixture(t)
	pin, err := first.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = checkInfrastructureTarget(t.Context(), first.store, pin); err != nil {
		t.Fatal(err)
	}
	if err = checkInfrastructureTarget(t.Context(), second.store, pin); !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("other root accepted as captured target: %v", err)
	}
	if pin.Root() != filepath.Clean(first.store.Root) {
		t.Fatal("target pin changed")
	}
}
