package lifetime

import (
	"errors"
	"os"
	"testing"
)

func TestLinuxPersistentPlacementRequiresUserManagerLinger(t *testing.T) {
	for _, test := range []struct {
		name, cgroup      string
		linger, wantError bool
	}{
		{"session", "0::/user.slice/user-1000.slice/session-3.scope", true, true},
		{"user-no-linger", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/owner.service", false, true},
		{"user-linger", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/owner.service", true, false},
		{"system", "0::/system.slice/delegate.service", false, false},
		{"container", "0::/", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireLinuxPersistentPlacement(test.cgroup, func(uid string) error {
				if uid != "1000" {
					t.Fatalf("user manager uid=%q", uid)
				}
				if !test.linger {
					return os.ErrNotExist
				}
				return nil
			})
			if (err != nil) != test.wantError {
				t.Fatalf("placement result=%v", err)
			}
			if test.name == "user-no-linger" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lost linger diagnostic: %v", err)
			}
		})
	}
}

func TestSystemdLingerNameMatchesNativeCEscape(t *testing.T) {
	if got := systemdLingerName("worker-1"); got != "worker-1" {
		t.Fatal(got)
	}
	if got := systemdLingerName("a\t'\\é"); got != `a\t\'\\\303\251` {
		t.Fatalf("C escape=%q", got)
	}
}
