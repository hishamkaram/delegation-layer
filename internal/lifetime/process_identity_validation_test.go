package lifetime

import (
	"errors"
	"os"
	"testing"
)

func TestProcessIdentityForHostAcceptsNativeObservation(t *testing.T) {
	ref, err := ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProcessIdentityForHost(ref); err != nil {
		t.Fatal(err)
	}
}

func TestProcessIdentityValidationRefusesUnknownAndForeignFormats(t *testing.T) {
	for _, test := range []struct {
		host, token string
		valid       bool
	}{
		{"linux", "linux:0", true},
		{"linux", "linux:18446744073709551615", true},
		{"linux", "linux:01", false},
		{"linux", "linux:-1", false},
		{"linux", "linux:18446744073709551616", false},
		{"linux", "opaque", false},
		{"linux", "darwin:1:2", false},
		{"darwin", "darwin:1:2", true},
		{"darwin", "darwin-v2:1:2:3", true},
		{"darwin", "darwin-v2:1:bad:3", false},
		{"darwin", "linux:1", false},
		{"unknown", "linux:1", false},
	} {
		err := validateProcessIdentityForOS(test.token, test.host)
		if (err == nil) != test.valid {
			t.Errorf("%s %q: %v", test.host, test.token, err)
		}
	}
	ref, err := NewProcessRef(1<<30, "unknown")
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProcessIdentityForHost(ref); !errors.Is(err, ErrInvalidLaunchRef) {
		t.Fatalf("unknown absent identity: %v", err)
	}
}

func TestNativeAbsenceValidatesObservedIdentityAndPreservesDarwinCompatibility(t *testing.T) {
	for _, test := range []struct {
		name, persisted, observed string
		absent, fault             bool
	}{
		{"legacy identifies current v2", "darwin:10:20", "darwin-v2:10:20:7", false, false},
		{"v2 identifies current legacy", "darwin-v2:10:20:7", "darwin:10:20", false, false},
		{"different incarnation", "darwin-v2:10:20:7", "darwin-v2:10:20:8", true, false},
		{"unknown observed incarnation", "darwin-v2:10:20:7", "opaque", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			absent, err := nativeProcessAbsentWith(42, test.persisted, "darwin", func(int) (bool, string, error) { return true, test.observed, nil })
			if absent != test.absent || (err != nil) != test.fault {
				t.Fatalf("absent=%t err=%v", absent, err)
			}
		})
	}
	called := false
	absent, err := nativeProcessAbsentWith(42, "unknown", "linux", func(int) (bool, string, error) { called = true; return false, "", nil })
	if absent || err == nil || called {
		t.Fatalf("unknown persisted identity bypassed validation: absent=%t err=%v called=%t", absent, err, called)
	}
}
