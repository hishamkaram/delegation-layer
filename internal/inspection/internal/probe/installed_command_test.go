package probe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestInstalledHelperReachesExactStartWithoutReadingProgram(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "installed-helper")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o111); err != nil {
		t.Fatal(err)
	}
	definition := provider.InspectionDefinition{Revision: "installed-helper-v1", Executable: executable, Directory: directory, Arguments: []string{"literal;argument"}, Environment: []string{"LITERAL=unchanged"}, OutputLimit: 64, Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	starts := 0
	refused := errors.New("actorless start refusal")
	_, err := runNative(nativeTestScope{ctx: ctx, authorize: func() error { return nil }}, definition, nativeHooks{start: func(command *exec.Cmd) error {
		starts++
		if command.Path != executable || !slices.Equal(command.Args, append([]string{executable}, definition.Arguments...)) || command.Dir != directory || !slices.Equal(command.Env, definition.Environment) {
			t.Errorf("installed selection changed before Start: %+v", command)
		}
		return refused
	}})
	if !errors.Is(err, errNativeInspection) || starts != 1 {
		t.Fatalf("installed helper did not reach its exact owned Start: starts=%d err=%v", starts, err)
	}
}

// The historical digest belongs to canonical definition data, not a fresh
// inspection compatibility gate. The owned lifetime refuses before native Start.
func TestInspectInstalledHelperReachesLifetimeWithoutReadingProgram(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "installed-helper")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o111); err != nil {
		t.Fatal(err)
	}
	definition := provider.InspectionDefinition{Revision: "historical-definition-v1", Executable: executable, ExecutableSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Directory: directory, Arguments: []string{"fixed"}, Environment: []string{}, OutputLimit: 64, Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }}
	lifetime := &probeStartLifetime{t: t, definition: definition, refusal: errors.New("actorless lifetime refusal")}
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime}, func(scope execution.PreflightScope) error {
		_, err := Inspect(scope, definition)
		return err
	})
	if !errors.Is(workErr, errNativeInspection) || stopErr != nil || len(lifetime.arguments()) != 1 {
		t.Fatalf("installed helper did not reach owned lifetime once: starts=%v work=%v stop=%v", lifetime.arguments(), workErr, stopErr)
	}
}
