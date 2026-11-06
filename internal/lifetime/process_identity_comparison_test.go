package lifetime

import (
	"errors"
	"testing"
)

func TestSameProcessUsesCanonicalIncarnationSemantics(t *testing.T) {
	for _, test := range []struct {
		name, observed, persisted string
		match                     bool
	}{
		{"opaque-equal", "opaque", "opaque", true},
		{"opaque-near-prefix", "darwin-v2-other:x", "darwin-v2-other:x", true},
		{"opaque-different", "opaque", "different", false},
		{"linux-equal", "linux:123", "linux:123", true},
		{"linux-different", "linux:123", "linux:124", false},
		{"darwin-v2-exec", "darwin-v2:10:20:7", "darwin-v2:11:21:7", true},
		{"darwin-v2-reused", "darwin-v2:10:20:7", "darwin-v2:10:20:8", false},
		{"darwin-legacy", "darwin:10:20", "darwin:10:20", true},
		{"darwin-legacy-changed", "darwin:10:20", "darwin:10:21", false},
		{"darwin-mixed", "darwin-v2:10:20:7", "darwin:10:20", true},
		{"darwin-mixed-reverse", "darwin:10:20", "darwin-v2:10:20:7", true},
		{"darwin-mixed-changed", "darwin-v2:11:21:7", "darwin:10:20", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed, err := NewProcessRef(7, test.observed)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := NewProcessRef(7, test.persisted)
			if err != nil {
				t.Fatal(err)
			}
			match, err := SameProcess(observed, persisted)
			if err != nil || match != test.match {
				t.Fatalf("match=%t want=%t err=%v", match, test.match, err)
			}
			differentPID, err := NewProcessRef(8, test.persisted)
			if err != nil {
				t.Fatal(err)
			}
			if match, err = SameProcess(observed, differentPID); err != nil || match {
				t.Fatalf("different PID matched: %v", err)
			}
		})
	}
}

func TestSameProcessRejectsInvalidReferences(t *testing.T) {
	valid, err := NewProcessRef(7, "opaque")
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []ProcessRef{{}, {pid: -1, startToken: "opaque"}, {pid: 7}, {pid: 7, startToken: "bad\x00token"}, {pid: 7, startToken: "darwin-v2:10:20:bad"}, {pid: 7, startToken: "darwin:10:bad"}} {
		for _, pair := range [][2]ProcessRef{{invalid, valid}, {valid, invalid}} {
			if match, compareErr := SameProcess(pair[0], pair[1]); !errors.Is(compareErr, ErrInvalidLaunchRef) || match {
				t.Fatalf("invalid reference accepted: match=%t err=%v", match, compareErr)
			}
		}
	}
}

func TestNewProcessRefRejectsMalformedRecognizedDarwinTokens(t *testing.T) {
	for _, token := range []string{"darwin:", "darwin:10", "darwin:10:x", "darwin:10:20:30", "darwin-v2:", "darwin-v2:10:20", "darwin-v2:10:20:x", "darwin-v2:10:20:30:40", "darwin-v2:-1:20:30", "darwin-v2:10::30"} {
		if _, err := NewProcessRef(7, token); !errors.Is(err, ErrInvalidLaunchRef) {
			t.Fatalf("malformed recognized token accepted: %v", err)
		}
	}
}
