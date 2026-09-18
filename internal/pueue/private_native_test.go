package pueue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateSupervisorNativeBootstrap(t *testing.T) {
	clientPath := os.Getenv("DELEGATE_TEST_PUEUE")
	daemonPath := os.Getenv("DELEGATE_TEST_PUEUED")
	if clientPath == "" || daemonPath == "" {
		t.Skip("native supervisor paths are not configured")
	}
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	client, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	shutdown := func() error {
		command := exec.Command(clientPath, "-c", client.Binding().ConfigPath, "shutdown")
		return command.Run()
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		if shutdownErr := shutdown(); shutdownErr != nil && !errors.Is(shutdownErr, os.ErrProcessDone) {
			t.Errorf("native private supervisor cleanup failed: %v", shutdownErr)
		}
	})

	binding := client.Binding()
	if strings.TrimSpace(binding.ObservedVersion) == "" {
		t.Fatal("native private supervisor did not report a version")
	}
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if !strings.HasPrefix(binding.ConfigPath, privateBase+string(filepath.Separator)) {
		t.Fatalf("private config escaped state root: %q", binding.ConfigPath)
	}
	if err := client.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := shutdown(); err != nil {
		t.Fatal(err)
	}
	stopped = true
}
