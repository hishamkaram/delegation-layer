package lifetime

import (
	"runtime"
	"strconv"
	"strings"
)

// NativeObservation supplies read-only boot and exact-process facts. The
// storage consumer owns its exclusion and persistence policy. The records and
// observations must refer to the same host and PID namespace.
type NativeObservation struct{}

func (NativeObservation) CurrentBootID() (string, error) { return BootID() }

func (NativeObservation) SameBoot(recorded, current string) bool {
	return CompareBootIDs(recorded, current) == BootSame
}

func (NativeObservation) ProcessAbsent(pid int, start string) (bool, error) {
	return nativeProcessAbsentWith(pid, start, runtime.GOOS, observeProcessIdentity)
}

func nativeProcessAbsentWith(pid int, start, host string, observe func(int) (bool, string, error)) (bool, error) {
	ref, err := NewProcessRef(pid, start)
	if err != nil {
		return false, err
	}
	if err = validateProcessIdentityForOS(ref.StartToken(), host); err != nil {
		return false, err
	}
	present, token, err := observe(pid)
	if err != nil || !present {
		return !present && err == nil, err
	}
	observed, err := NewProcessRef(pid, token)
	if err != nil {
		return false, err
	}
	if err = validateProcessIdentityForOS(observed.StartToken(), host); err != nil {
		return false, err
	}
	same, err := SameProcess(observed, ref)
	return !same && err == nil, err
}

// ValidateProcessIdentityForHost validates an incarnation before an absence
// observation. Unlike the compatibility readers, it refuses opaque or foreign
// tokens even when the PID is absent. It observes and controls no process.
func ValidateProcessIdentityForHost(ref ProcessRef) error {
	if err := validateProcessRef(ref); err != nil {
		return err
	}
	return validateProcessIdentityForOS(ref.startToken, runtime.GOOS)
}

func validateProcessIdentityForOS(token, host string) error {
	switch host {
	case "linux":
		value, found := strings.CutPrefix(token, "linux:")
		ticks, err := strconv.ParseUint(value, 10, 64)
		if found && err == nil && strconv.FormatUint(ticks, 10) == value {
			return nil
		}
	case "darwin":
		for _, format := range []struct {
			prefix string
			parts  int
		}{{"darwin:", 2}, {"darwin-v2:", 3}} {
			if _, valid := parseDarwinIdentityToken(token, format.prefix, format.parts); valid {
				return nil
			}
		}
	}
	return ErrInvalidLaunchRef
}
