package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// allowDaemonStartForTest authorizes only unit fixtures, without proving native placement.
type allowDaemonStartForTest struct{}

func (allowDaemonStartForTest) GuardDaemonStart(_ context.Context, _ string, start func() error) error {
	return start()
}

type daemonStartGuardFunc func(context.Context, string, func() error) error

func (f daemonStartGuardFunc) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return f(ctx, root, start)
}

type nilDaemonStartGuardForTest struct{}

func (g *nilDaemonStartGuardForTest) GuardDaemonStart(_ context.Context, _ string, start func() error) error {
	if g == nil {
		return ErrInfrastructureOwnerRequired
	}
	return start()
}

func TestDaemonStartRequiresGuard(t *testing.T) {
	for _, guard := range []DaemonStartGuard{nil, (*nilDaemonStartGuardForTest)(nil), daemonStartGuardFunc(func(context.Context, string, func() error) error { return nil })} {
		t.Run("refuses", func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "started")
			executable := filepath.Join(root, "daemon")
			writeExecutable(t, executable, "#!/bin/sh\ntouch '"+marker+"'\n")
			err = startDaemon(t.Context(), root, executable, filepath.Join(root, "config"), root, Options{DaemonStartGuard: guard})
			if !errors.Is(err, ErrInfrastructureOwnerRequired) {
				t.Fatalf("guard refusal = %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("child started: %v", err)
			}
		})
	}
}

func TestDaemonStartGuardCarriesContextRootAndExpires(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	refusal := errors.New("placement refused")
	var retained func() error
	guard := daemonStartGuardFunc(func(got context.Context, gotRoot string, start func() error) error {
		if got != ctx || gotRoot != root {
			t.Fatalf("guard inputs changed: ctx=%v root=%q", got == ctx, gotRoot)
		}
		retained = start
		return refusal
	})
	cmd := exec.Command(filepath.Join(root, "missing"))
	started, err := guardedDaemonStart(ctx, root, filepath.Join(root, "external.yml"), cmd, guard, nil)
	if started.done != nil || started.birth != nil || !errors.Is(err, refusal) {
		t.Fatalf("refusal: started=%t err=%v", started.done != nil, err)
	}
	if err := retained(); !errors.Is(err, ErrInfrastructureOwnerRequired) {
		t.Fatalf("retained callback = %v", err)
	}
	if cmd.Process != nil {
		t.Fatal("refused callback started child")
	}
}

func TestDaemonStartPreservesIgnoredStartFailure(t *testing.T) {
	root := t.TempDir()
	guard := daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		if err := start(); err == nil {
			t.Fatal("missing executable started")
		}
		return nil
	})
	started, err := guardedDaemonStart(t.Context(), root, filepath.Join(root, "config"), exec.Command(filepath.Join(root, "missing")), guard, nil)
	if started.done != nil || started.birth != nil || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, ErrBinding) {
		t.Fatalf("start failure: started=%t err=%v", started.done != nil, err)
	}
}

func TestDaemonStartRejectsDifferentPrivateRoot(t *testing.T) {
	root, _ := newPrivateSupervisorTestRoots(t)
	other, _ := newPrivateSupervisorTestRoots(t)
	for _, config := range []string{PrivateConfigPath(root), privateRecoveryConfigPath(root)} {
		guard := daemonStartGuardFunc(func(context.Context, string, func() error) error {
			t.Fatal("mismatched config reached guard")
			return nil
		})
		started, err := guardedDaemonStart(t.Context(), other, config, exec.Command("missing"), guard, nil)
		if started.done != nil || started.birth != nil || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("mismatched config: started=%t err=%v", started.done != nil, err)
		}
	}
}
