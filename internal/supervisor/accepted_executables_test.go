package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestAcceptedSupervisorPairKeepsDifferentlyNamedInstalledClient(t *testing.T) {
	directory := t.TempDir()
	versioned := filepath.Join(directory, "pueue-4.0.4")
	planted, daemon := filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	for _, path := range []string{versioned, planted, daemon} {
		selectionExecutable(t, path)
	}
	bin := t.TempDir()
	alias := filepath.Join(bin, "pueue")
	if err := os.Symlink(versioned, alias); err != nil {
		t.Fatal(err)
	}
	lookupFromPath := ExecutableLookup{Environment: []string{"PATH=" + bin}}
	fromPath, err := lookupFromPath.lookPath("pueue")
	if err != nil || fromPath != alias {
		t.Fatalf("captured PATH alias=%q err=%v", fromPath, err)
	}
	selected, err := resolveInstalledSupervisorClient(ExecutableLookup{InitialExecutable: fromPath})
	if err != nil || selected != versioned {
		t.Fatalf("selected alias=%q err=%v", selected, err)
	}
	lookup := ExecutableLookup{InitialExecutable: selected, AcceptedBootstrapPair: BootstrapPair{Client: selected, Daemon: daemon}}
	client, actualDaemon, err := ResolvePrivateSupervisorExecutables(t.TempDir(), lookup)
	if err != nil || client != versioned || actualDaemon != daemon {
		t.Fatalf("accepted client was reselected: %q %q %v", client, actualDaemon, err)
	}
	replacement := filepath.Join(directory, "different-native-client")
	selectionExecutable(t, replacement)
	if err = os.Remove(versioned); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(replacement, versioned); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ResolvePrivateSupervisorExecutables(t.TempDir(), lookup); !errors.Is(err, ErrSupervisorSelectionChanged) || !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("accepted path retargeted: %v", err)
	}
}
