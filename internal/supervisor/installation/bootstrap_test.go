package installation

import (
	"testing"
)

func TestAcceptedSupervisorPairDoesNotUseUnacceptedInstallation(t *testing.T) {
	client, daemon := installedPairFixture(t, "fixture client", "fixture daemon")
	lookup := ExecutableLookup{InitialExecutable: "/unaccepted-install/pueue", AcceptedBootstrapPair: BootstrapPair{Client: client, Daemon: daemon}}
	gotClient, gotDaemon, err := ResolvePrivateSupervisorExecutables(lookup)
	if err != nil || gotClient != client || gotDaemon != daemon {
		t.Fatalf("accepted installed selection lost: %q %q %v", gotClient, gotDaemon, err)
	}
}
