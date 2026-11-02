package lifetime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestNestedFixtureHistoricalIdentityUsesDomainEquality(t *testing.T) {
	cases := []struct {
		name, observed, persisted string
		matches                   bool
	}{
		{"darwin-exec", "darwin-v2:100:200:42", "darwin-v2:101:201:42", true},
		{"darwin-reused-pid", "darwin-v2:100:200:43", "darwin-v2:100:200:42", false},
		{"legacy-compatible", "darwin-v2:100:200:42", "darwin:100:200", true},
		{"legacy-changed", "darwin-v2:101:200:42", "darwin:100:200", false},
		{"linux-same", "linux:11", "linux:11", true},
		{"linux-reused-pid", "linux:12", "linux:11", false},
	}
	for _, check := range cases {
		t.Run(check.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "comparison.json")
			t.Setenv("DELEGATE_NESTED_FIXTURE_OUTPUT", output)
			t.Setenv("DELEGATE_NESTED_FIXTURE_OBSERVED_TOKEN", check.observed)
			t.Setenv("DELEGATE_NESTED_FIXTURE_EXPECTED_TOKEN", check.persisted)
			if err := runNestedNativeFixture("compare-identity"); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Matches bool `json:"identity_matches"`
			}
			if err = json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Matches != check.matches {
				t.Fatalf("domain equality = %v, want %v", result.Matches, check.matches)
			}
		})
	}
}

func TestNestedFixtureCurrentObservationMatchesExpectedIncarnation(t *testing.T) {
	observation, err := nestedObserveNative(os.Getpid(), "")
	if err != nil || !observation.Present {
		t.Fatalf("observe test process: present=%v error=%v", observation.Present, err)
	}
	output := filepath.Join(t.TempDir(), "current.json")
	t.Setenv("DELEGATE_NESTED_FIXTURE_OUTPUT", output)
	t.Setenv("DELEGATE_NESTED_FIXTURE_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("DELEGATE_NESTED_FIXTURE_EXPECTED_TOKEN", observation.StartToken)
	if err = runNestedNativeFixture("inspect"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Present bool `json:"present"`
		Matches bool `json:"identity_matches"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Present || !result.Matches {
		t.Fatalf("current incarnation lost: %+v", result)
	}
}
