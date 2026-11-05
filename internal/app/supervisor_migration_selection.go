package app

import (
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
)

func newSupervisorCommandResponse(action string) Response {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{
		Action: action, State: "unknown", LegacyReachability: "unknown",
		ActiveWork: "unknown", NextAction: "stop_and_report",
	}
	return response
}

// The accepted startup intent owns resident selection. A resident must never
// rediscover an installation from the directory containing its staged client.
// Ordinary CLI migration retains its current-installation selection policy.
func migrationSupervisorPair(root string, lookup supervisor.ExecutableLookup) (string, string, error) {
	accepted := lookup.AcceptedBootstrapPair
	if accepted == (supervisor.BootstrapPair{}) {
		return supervisor.ResolveCurrentPrivateSupervisorExecutables(root, lookup)
	}
	for _, path := range []string{accepted.Client, accepted.Daemon} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", "", pueue.ErrConfiguration
		}
	}
	return accepted.Client, accepted.Daemon, nil
}
