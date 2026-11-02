//go:build darwin

package lifetime

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

func nestedObserveNative(pid int, scope string) (nestedNativeObservation, error) {
	result := nestedNativeObservation{PID: pid}
	process, err := nativeDarwinProcessKinfoLookup(pid)
	if err != nil || process == nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snapshot, present, err := snapshotDarwinProcess(ctx, scope, *process, uint32(unix.Geteuid()), true)
	if err != nil || !present {
		return result, err
	}
	result.Present, result.StartToken = !snapshot.zombie, snapshot.startToken
	result.ParentPID, result.GroupID, result.SessionID = snapshot.parentPID, snapshot.pgid, snapshot.sessionID
	result.ScopeMarked = snapshot.scopeMarked
	return result, nil
}
