//go:build linux

package pueue

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func isLinuxPrivateProcessAbsentError(err error) bool {
	return errors.Is(err, unix.ESRCH) || errors.Is(err, os.ErrNotExist)
}

func observeNativePrivateProcess(pid int) (privateProcessObservation, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if isLinuxPrivateProcessAbsentError(err) {
		return privateProcessObservation{}, nil
	}
	if err != nil {
		return privateProcessObservation{}, fmt.Errorf("read private supervisor process identity: %w", err)
	}
	return parseLinuxPrivateProcessStat(data)
}

func parseLinuxPrivateProcessStat(data []byte) (privateProcessObservation, error) {
	text := strings.TrimSpace(string(data))
	closing := strings.LastIndex(text, ")")
	if closing < 0 || closing+2 > len(text) || text[closing+1] != ' ' {
		return privateProcessObservation{}, errors.New("private supervisor process stat is malformed")
	}
	fields := strings.Fields(text[closing+2:])
	// The suffix begins at proc(5) field 3 (state); starttime is field 22.
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return privateProcessObservation{}, errors.New("private supervisor process stat is incomplete")
	}
	if fields[0] == "Z" {
		return privateProcessObservation{}, nil
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || strconv.FormatUint(start, 10) != fields[19] {
		return privateProcessObservation{}, errors.New("private supervisor process start token is invalid")
	}
	return privateProcessObservation{Present: true, StartToken: "linux:" + fields[19]}, nil
}
