//go:build darwin

package pueue

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

const darwinZombieProcessState = 5

func observeNativePrivateProcess(pid int) (privateProcessObservation, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return privateProcessObservation{}, fmt.Errorf("inspect private supervisor process identity: %w", err)
	}
	if len(processes) == 0 {
		return privateProcessObservation{}, nil
	}
	if len(processes) != 1 || processes[0].Proc.P_pid != int32(pid) {
		return privateProcessObservation{}, fmt.Errorf("private supervisor process identity is ambiguous")
	}
	process := processes[0].Proc
	if process.P_stat == darwinZombieProcessState {
		return privateProcessObservation{}, nil
	}
	start := process.P_starttime
	token := "darwin:" + strconv.FormatInt(start.Sec, 10) + ":" + strconv.FormatInt(int64(start.Usec), 10)
	return privateProcessObservation{Present: true, StartToken: token}, nil
}
