package lifetime

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// RequirePersistentProcess rejects a verified resident placed in a disposable
// systemd login scope. It does not change placement or configure the host.
func RequirePersistentProcess(expected ProcessRef) error {
	before, err := ObserveProcess(expected.PID())
	if err != nil {
		return err
	}
	same, err := SameProcess(before, expected)
	if err != nil {
		return err
	}
	if !same {
		return ErrInvalidRef
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(expected.PID()) + "/cgroup")
	if err != nil {
		return err
	}
	after, err := ObserveProcess(expected.PID())
	if err != nil {
		return err
	}
	same, err = SameProcess(after, expected)
	if err != nil {
		return err
	}
	if !same {
		return ErrInvalidRef
	}
	return requireLinuxPersistentPlacement(string(data), requireLinuxLinger)
}

func requireLinuxPersistentPlacement(cgroups string, requireLinger func(string) error) error {
	for _, line := range strings.Split(cgroups, "\n") {
		for _, part := range strings.Split(line, "/") {
			if err := requireLinuxPersistentComponent(part, requireLinger); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireLinuxPersistentComponent(part string, requireLinger func(string) error) error {
	if strings.HasPrefix(part, "session-") && strings.HasSuffix(part, ".scope") {
		return fmt.Errorf("login_bound_supervisor: run supervisor ensure-owner from a prepared persistent user context")
	}
	uid, userManager := strings.CutPrefix(part, "user@")
	if !userManager {
		return nil
	}
	uid, service := strings.CutSuffix(uid, ".service")
	if !service {
		return nil
	}
	if _, err := strconv.ParseUint(uid, 10, 32); err != nil {
		return fmt.Errorf("login_bound_supervisor: cannot verify user manager identity: %w", err)
	}
	if err := requireLinger(uid); err != nil {
		return fmt.Errorf("login_bound_supervisor: user manager must already persist after logout: %w", err)
	}
	return nil
}

func requireLinuxLinger(uid string) error {
	account, err := user.LookupId(uid)
	if err != nil {
		return err
	}
	// systemd's logind-user.c uses cescape(username) under this directory.
	// Observe only; enabling linger and host placement remain user-owned.
	name := systemdLingerName(account.Username)
	if name == "" || strings.Contains(name, "/") || name == "." || name == ".." {
		return fmt.Errorf("cannot verify systemd linger account name")
	}
	_, err = os.Stat(filepath.Join("/var/lib/systemd/linger", name))
	return err
}

func systemdLingerName(name string) string {
	var escaped string
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch c {
		case '\a', '\b', '\f', '\n', '\r', '\t', '\v':
			escaped += map[byte]string{'\a': `\a`, '\b': `\b`, '\f': `\f`, '\n': `\n`, '\r': `\r`, '\t': `\t`, '\v': `\v`}[c]
		case '\\', '"', '\'':
			escaped += "\\" + string(c)
		default:
			if c < ' ' || c >= 127 {
				escaped += fmt.Sprintf("\\%03o", c)
			} else {
				escaped += string(c)
			}
		}
	}
	return escaped
}
