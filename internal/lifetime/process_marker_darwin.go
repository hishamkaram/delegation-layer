//go:build darwin

package lifetime

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

const darwinProcessMarkerReadAttempts = 3

func darwinProcessHasScopeMarker(ctx context.Context, pid int, expectedStartToken, identifier string, strict bool) (bool, error) {
	return darwinProcessHasScopeMarkerWith(
		ctx,
		pid,
		expectedStartToken,
		identifier,
		strict,
		func(pid int) ([]byte, error) {
			return unix.SysctlRaw("kern.procargs2", pid)
		},
		inspectDarwinProcessIdentity,
		sleepContext,
	)
}

func darwinProcessHasScopeMarkerWith(
	ctx context.Context,
	pid int,
	expectedStartToken string,
	identifier string,
	strict bool,
	readProcargs func(int) ([]byte, error),
	inspectIdentity func(int) (bool, bool, string, error),
	waitRetry sleepFunc,
) (bool, error) {
	expected := processScopeMarkerEnvironment + "=" + identifier
	retrying := false
	for attempt := 0; attempt < darwinProcessMarkerReadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		data, err := readProcargs(pid)
		if err == nil {
			return parseDarwinProcessScopeMarker(pid, expectedStartToken, expected, retrying, data, inspectIdentity)
		}
		if !retryableDarwinProcessMarkerError(err) {
			return classifyDarwinProcessMarkerError(pid, err, strict)
		}
		retrying = true
		matches, matchErr := darwinProcessIdentityMatches(pid, expectedStartToken, inspectIdentity)
		if matchErr != nil || !matches {
			return false, matchErr
		}
		if attempt == darwinProcessMarkerReadAttempts-1 {
			return classifyDarwinProcessMarkerError(pid, err, strict)
		}
		if err = waitRetry(ctx, processScopePollInterval); err != nil {
			return false, err
		}
	}
	return false, errors.New("darwin process marker read exhausted without a result")
}

func parseDarwinProcessScopeMarker(
	pid int,
	expectedStartToken string,
	expected string,
	retrying bool,
	data []byte,
	inspectIdentity func(int) (bool, bool, string, error),
) (bool, error) {
	if retrying {
		matches, err := darwinProcessIdentityMatches(pid, expectedStartToken, inspectIdentity)
		if err != nil || !matches {
			return false, err
		}
	}
	if !bytes.Contains(data, []byte(expected)) {
		return false, nil
	}
	marked, err := darwinProcessEnvironmentContains(data, expected)
	if err != nil {
		return false, fmt.Errorf("parse Darwin environment for PID %d: %w", pid, err)
	}
	return marked, nil
}

func retryableDarwinProcessMarkerError(err error) bool {
	return errors.Is(err, unix.EIO) || errors.Is(err, unix.EINVAL)
}

func darwinProcessIdentityMatches(
	pid int,
	expectedStartToken string,
	inspectIdentity func(int) (bool, bool, string, error),
) (bool, error) {
	present, zombie, startToken, err := inspectIdentity(pid)
	if err != nil {
		return false, fmt.Errorf("reinspect Darwin identity for PID %d: %w", pid, err)
	}
	return present && !zombie && processIdentityTokensMatch(startToken, expectedStartToken), nil
}

func classifyDarwinProcessMarkerError(pid int, err error, strict bool) (bool, error) {
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EIO) || errors.Is(err, unix.EINVAL) {
		if strict {
			return false, fmt.Errorf("%w: inspect Darwin environment for PID %d: %w", ErrUnverifiableLaunch, pid, err)
		}
		// Cooperative recovery still owns exact launch identities and connected
		// topology when Darwin omits or cannot copy an unrelated process's
		// environment. Marker discovery is supplemental after a launch. Before a
		// launch, strict pending-start proof continues to fail closed.
		return false, nil
	}
	return false, fmt.Errorf("inspect Darwin environment for PID %d: %w", pid, err)
}

func darwinProcessEnvironmentContains(data []byte, expected string) (bool, error) {
	offset, err := darwinProcessEnvironmentOffset(data, expected)
	if err != nil {
		return false, err
	}
	return darwinProcessEnvironmentEntryContains(data, offset, expected)
}

func darwinProcessEnvironmentOffset(data []byte, expected string) (int, error) {
	const argumentCountSize = 4
	if len(data) < argumentCountSize || expected == "" || strings.IndexByte(expected, 0) >= 0 {
		return 0, errors.New("darwin procargs record is malformed")
	}
	argumentCount := int(int32(binary.LittleEndian.Uint32(data[:argumentCountSize])))
	if argumentCount < 0 || argumentCount > len(data)-argumentCountSize {
		return 0, errors.New("darwin procargs argument count is invalid")
	}
	offset, err := skipDarwinProcargsValue(data, argumentCountSize)
	if err != nil {
		return 0, errors.New("darwin procargs executable path is unterminated")
	}
	offset = skipDarwinProcargsNulls(data, offset)
	for argument := 0; argument < argumentCount; argument++ {
		offset, err = skipDarwinProcargsValue(data, offset)
		if err != nil {
			return 0, errors.New("darwin procargs argument is unterminated")
		}
	}
	return skipDarwinProcargsNulls(data, offset), nil
}

func darwinProcessEnvironmentEntryContains(data []byte, offset int, expected string) (bool, error) {
	for offset < len(data) {
		end := bytes.IndexByte(data[offset:], 0)
		if end < 0 {
			return false, errors.New("darwin procargs environment is unterminated")
		}
		if string(data[offset:offset+end]) == expected {
			return true, nil
		}
		offset = skipDarwinProcargsNulls(data, offset+end+1)
	}
	return false, nil
}

func skipDarwinProcargsValue(data []byte, offset int) (int, error) {
	end := bytes.IndexByte(data[offset:], 0)
	if end < 0 {
		return 0, errors.New("darwin procargs value is unterminated")
	}
	return offset + end + 1, nil
}

func skipDarwinProcargsNulls(data []byte, offset int) int {
	for offset < len(data) && data[offset] == 0 {
		offset++
	}
	return offset
}
