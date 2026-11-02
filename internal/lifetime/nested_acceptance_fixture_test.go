package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// This test-binary entrypoint keeps acceptance OS observations and deliberate
// orphan creation beside the native lifetime owner. It is never shipped.
type nestedNativeObservation struct {
	PID             int    `json:"pid"`
	Present         bool   `json:"present"`
	StartToken      string `json:"process_start"`
	ParentPID       int    `json:"ppid"`
	GroupID         int    `json:"pgid"`
	SessionID       int    `json:"session_id"`
	Cgroup          string `json:"cgroup"`
	ScopeMarked     bool   `json:"scope_marked"`
	OrphanedFrom    int    `json:"orphaned_from,omitempty"`
	ScopePresent    bool   `json:"persistent_scope_present"`
	IdentityMatches bool   `json:"identity_matches"`
}

func TestNestedSurvivalFixture(t *testing.T) {
	mode := os.Getenv("DELEGATE_NESTED_FIXTURE_MODE")
	if mode == "" {
		t.Skip("entrypoint for the bounded nested-survival acceptance driver")
	}
	if err := runNestedNativeFixture(mode); err != nil {
		t.Fatal(err)
	}
}

func runNestedNativeFixture(mode string) error {
	switch mode {
	case "inspect":
		pid, err := strconv.Atoi(os.Getenv("DELEGATE_NESTED_FIXTURE_PID"))
		if err != nil || pid <= 1 {
			return errors.New("invalid nested fixture observation PID")
		}
		observation, err := nestedObserveNative(pid, os.Getenv("DELEGATE_NESTED_FIXTURE_SCOPE"))
		if err != nil {
			return err
		}
		observation.IdentityMatches = observation.Present && nestedFixtureTokensMatch(
			observation.StartToken, os.Getenv("DELEGATE_NESTED_FIXTURE_EXPECTED_TOKEN"))
		return writeNestedFixtureObservation(observation)
	case "compare-identity":
		return writeNestedFixtureObservation(nestedNativeObservation{
			IdentityMatches: nestedFixtureTokensMatch(
				os.Getenv("DELEGATE_NESTED_FIXTURE_OBSERVED_TOKEN"), os.Getenv("DELEGATE_NESTED_FIXTURE_EXPECTED_TOKEN")),
		})
	case "orphan-parent":
		return startNestedOrphan()
	case "scope":
		return observeNestedScope()
	case "orphan-child":
		return awaitNestedOrphanRelease()
	default:
		return errors.New("unknown nested fixture mode")
	}
}

// Empty inputs are not an identity observation. Platform equivalence stays
// with the production owner, including Darwin exec and legacy compatibility.
func nestedFixtureTokensMatch(observed, persisted string) bool {
	return observed != "" && persisted != "" && processIdentityTokensMatch(observed, persisted)
}

func observeNestedScope() error {
	kind := Kind(os.Getenv("DELEGATE_NESTED_FIXTURE_KIND"))
	assurance := Assurance(os.Getenv("DELEGATE_NESTED_FIXTURE_ASSURANCE"))
	plan, err := NewPlan(kind, assurance, os.Getenv("DELEGATE_NESTED_FIXTURE_PLAN"))
	if err != nil {
		return err
	}
	ref, err := NewRef(kind, assurance, os.Getenv("DELEGATE_NESTED_FIXTURE_SCOPE_ID"))
	if err != nil {
		return err
	}
	result := nestedNativeObservation{}
	if kind == KindCgroupV2 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		scope, openErr := OpenRegisteredContext(ctx, plan, ref)
		if openErr != nil && !errors.Is(openErr, ErrScopeAbsent) {
			return openErr
		}
		if scope != nil {
			result.ScopePresent = true
			if err = scope.Release(); err != nil {
				return err
			}
		}
	}
	return writeNestedFixtureObservation(result)
}

func writeNestedFixtureObservation(observation nestedNativeObservation) error {
	return publishNativeFixtureRecord(os.Getenv("DELEGATE_NESTED_FIXTURE_OUTPUT"), false, func(writer io.Writer) error {
		return json.NewEncoder(writer).Encode(observation)
	})
}

func startNestedOrphan() error {
	command := exec.Command(os.Args[0], "-test.run=^TestNestedSurvivalFixture$")
	command.Env = append(os.Environ(), "DELEGATE_NESTED_FIXTURE_MODE=orphan-child",
		"DELEGATE_NESTED_FIXTURE_PARENT_PID="+strconv.Itoa(os.Getpid()))
	if err := ConfigureIndependentSession(command); err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	// Deliberately orphan this finite descendant. The leader's existing native
	// scope owns termination; the child also has an I/O release and finite timer.
	return command.Process.Release()
}

func awaitNestedOrphanRelease() error {
	deadlineNS, err := strconv.ParseInt(os.Getenv("DELEGATE_NESTED_FIXTURE_DEADLINE_NS"), 10, 64)
	if err != nil {
		return err
	}
	deadline := time.Unix(0, deadlineNS)
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 10*time.Minute {
		return errors.New("invalid nested descendant backstop")
	}
	observation, err := nestedObserveNative(os.Getpid(), "")
	if err != nil {
		return err
	}
	observation.OrphanedFrom, err = strconv.Atoi(os.Getenv("DELEGATE_NESTED_FIXTURE_PARENT_PID"))
	if err != nil {
		return err
	}
	if err = writeNestedFixtureObservation(observation); err != nil {
		return err
	}
	for time.Now().Before(deadline) {
		_, err = os.Stat(os.Getenv("DELEGATE_NESTED_FIXTURE_RELEASE"))
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}
