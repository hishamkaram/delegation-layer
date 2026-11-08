package app

import (
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func newSupervisorCommandResponse(action string) Response {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{
		Action: action, State: "unknown", LegacyReachability: "unknown",
		ActiveWork: "unknown", NextAction: "stop_and_report",
	}
	return response
}

// The accepted context owns resident selection. Its pair is derived from the
// selected installed client; no mutable state inventory or program copy is read.
// Ordinary CLI migration retains its current-installation selection policy.
func migrationSupervisorPair(lookup installation.ExecutableLookup) (string, string, error) {
	accepted := lookup.AcceptedBootstrapPair
	if accepted == (installation.BootstrapPair{}) {
		return installation.ResolvePrivateSupervisorExecutables(lookup)
	}
	for _, path := range []string{accepted.Client, accepted.Daemon} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", "", pueue.ErrConfiguration
		}
	}
	return accepted.Client, accepted.Daemon, nil
}
