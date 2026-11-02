//go:build linux

package lifetime

import (
	"errors"
	"os"
	"strconv"
)

func nestedObserveNative(pid int, _ string) (nestedNativeObservation, error) {
	result := nestedNativeObservation{PID: pid}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	parsed, err := parseLinuxProcessScopeStat(data)
	if err != nil {
		return result, err
	}
	snapshot := parsed.snapshot
	result.Present, result.StartToken = !parsed.zombie, snapshot.startToken
	result.ParentPID, result.GroupID, result.SessionID = snapshot.parentPID, snapshot.pgid, snapshot.sessionID
	data, err = os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if errors.Is(err, os.ErrNotExist) {
		result.Present = false
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Cgroup, err = parseCgroupV2Path(data)
	if err == nil && result.Present {
		ref, refErr := NewProcessRef(pid, result.StartToken)
		if refErr != nil {
			return result, refErr
		}
		result.Present, err = ProcessPresent(ref)
	}
	return result, err
}
