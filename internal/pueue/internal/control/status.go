package control

import "github.com/hishamkaram/delegation-layer/internal/pueue/protocol"

func snapshotFromResult(result CommandResult, version string) (QueueSnapshot, error) {
	if result.Err != nil {
		return QueueSnapshot{}, result.Err
	}
	if snapshot, ok := result.Snapshot(); ok {
		return snapshot, nil
	}
	return ParseQueueSnapshot(result.Stdout, version)
}

func groupsFromResult(result CommandResult) (map[string]Group, error) {
	if result.Err != nil {
		return nil, result.Err
	}
	return protocol.ParseGroups(result.Stdout)
}
