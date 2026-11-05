//go:build darwin

package lifetime

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

func nestedObserveNative(pid int, scope string) (nestedNativeObservation, error) {
	return nestedObserveNativeContext(context.Background(), pid, scope)
}

func nestedObserveNativeContext(ctx context.Context, pid int, scope string) (nestedNativeObservation, error) {
	result := nestedNativeObservation{PID: pid}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	process, err := nativeDarwinProcessKinfoLookup(pid)
	if err != nil || process == nil {
		return result, err
	}
	snapshot, present, err := snapshotDarwinProcess(ctx, scope, *process, uint32(unix.Geteuid()), true)
	if err != nil || !present {
		return result, err
	}
	result.Present, result.StartToken = !snapshot.zombie, snapshot.startToken
	result.ParentPID, result.GroupID, result.SessionID = snapshot.parentPID, snapshot.pgid, snapshot.sessionID
	result.ScopeMarked = snapshot.scopeMarked
	return result, nil
}
