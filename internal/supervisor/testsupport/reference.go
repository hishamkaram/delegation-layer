package testsupport

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ContinuationSupervisorRef(root, digest, endpoint string) task.SupervisorRef {
	return task.SupervisorRef{
		ClientExecutable:     "/usr/bin/true",
		ClientSHA256:         digest,
		DaemonExecutable:     "/usr/bin/true",
		DaemonSHA256:         digest,
		ResolvedConfigSHA256: digest,
		ConfigPath:           pueue.PrivateConfigPath(root),
		ConfigDigest:         digest,
		Endpoint:             endpoint,
		ObservedVersion:      pueue.FixtureVersion,
	}
}
