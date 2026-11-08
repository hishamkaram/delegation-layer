package installation

import "runtime/debug"

const (
	managedRunnerName = "delegate-run"
	managedRunnerMain = "github.com/hishamkaram/delegation-layer/cmd/delegate-run"
)

func ManagedRunnerBuildInfo(info *debug.BuildInfo) bool {
	return info != nil && info.Path == managedRunnerMain
}
