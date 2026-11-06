package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Historical fixture data only: no daemon or native owner exists behind these
// bindings. Production publication remains guarded by the canonical owner.
// The record shape matches control.privateSupervisorBindingRecord; readback
// through its public reader checks that the fixture is actually usable data.
func writeHistoricalSupervisorBinding(t *testing.T, root string, binding task.SupervisorRef) {
	t.Helper()
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int                `json:"schema_version"`
		Binding       task.SupervisorRef `json:"binding"`
	}{task.SchemaVersion, binding})
	if err != nil {
		t.Fatal(err)
	}
	// Historical readers retain the canonical bootstrap SH even though these
	// fixtures have no native daemon. Initialize its real lock through its owner.
	err = pueue.WithBootstrapLockContext(t.Context(), root, func(context.Context) error {
		file, openErr := os.OpenFile(filepath.Join(root, ".supervisor", "supervisor.identity.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			return openErr
		}
		_, writeErr := file.Write(data)
		return errors.Join(writeErr, file.Close())
	})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil || !found || got != binding {
		t.Fatalf("historical binding fixture does not round-trip canonically: found=%t err=%v", found, err)
	}
}
