package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/provider"
)

type nativeTestScope struct {
	ctx           context.Context
	authorize     func() error
	startCommand  func(*exec.Cmd, func() error) error
	finishCommand func(*exec.Cmd) error
}

func (s nativeTestScope) Context() context.Context { return s.ctx }

func (s nativeTestScope) StartCommand(cmd *exec.Cmd, start func() error) error {
	if s.ctx == nil || s.ctx.Err() != nil || s.authorize == nil || cmd == nil || start == nil {
		return errNativeInspection
	}
	if err := s.authorize(); err != nil {
		return err
	}
	if s.startCommand != nil {
		return s.startCommand(cmd, start)
	}
	return start()
}

func (s nativeTestScope) FinishCommand(cmd *exec.Cmd) error {
	if s.finishCommand != nil {
		return s.finishCommand(cmd)
	}
	return nil
}

func captureDefinition(t *testing.T) provider.InspectionDefinition {
	t.Helper()
	info, err := provider.LocateCLI("sh")
	if err != nil {
		t.Fatal(err)
	}
	return provider.InspectionDefinition{
		Revision: "native-test-v1", Executable: info.Path, ExecutableSHA256: info.SHA256,
		Directory: "/workspace", Arguments: []string{"fixed"}, Environment: []string{"PATH=/usr/bin"}, OutputLimit: 64,
		Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"eligible":true}`), nil },
	}
}

func TestNativeCaptureOwnsCommandAndMasksDiagnostics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	starts, waits, gates := 0, 0, 0
	definition := captureDefinition(t)
	data, err := runNative(nativeTestScope{ctx: ctx, authorize: func() error { gates++; return nil }}, definition, nativeHooks{
		start: func(cmd *exec.Cmd) error {
			starts++
			if cmd.Dir != "/workspace" || strings.Join(cmd.Env, ",") != "PATH=/usr/bin" || strings.Join(cmd.Args, ",") != definition.Executable+",fixed" || cmd.Stdin != nil {
				t.Fatal("command did not preserve the compiled description and finite stdin")
			}
			if runtime.GOOS == "linux" && cmd.Path != "/dev/fd/3" {
				t.Fatalf("Linux verified command path = %q, want /dev/fd/3", cmd.Path)
			}
			if runtime.GOOS != "linux" && cmd.Path == definition.Executable {
				t.Fatalf("portable verified command resolved the mutable executable path: %q", cmd.Path)
			}
			_, stdoutErr := io.WriteString(cmd.Stdout, "private-native-value")
			_, stderrErr := io.WriteString(cmd.Stderr, "discarded-private-diagnostic")
			return errors.Join(stdoutErr, stderrErr)
		},
		wait: func(*exec.Cmd) error { waits++; return nil },
	})
	defer clear(data)
	if err != nil || string(data) != "private-native-value" || starts != 1 || waits != 1 || gates != 1 {
		t.Fatalf("capture result: error=%v starts=%d waits=%d gates=%d", err, starts, waits, gates)
	}
}

type nativeCaptureLifetimeCase struct {
	name       string
	startErr   error
	waitErr    error
	finishErr  error
	wantError  bool
	wantEvents string
}

func TestNativeCaptureUsesLifetimeForExactCommand(t *testing.T) {
	for _, test := range []nativeCaptureLifetimeCase{
		{name: "success", wantEvents: "scope-start,start,wait,scope-finish"},
		{name: "start-failure", startErr: errors.New("private-start-failure"), wantError: true, wantEvents: "scope-start,start"},
		{name: "wait-failure", waitErr: errors.New("private-wait-failure"), wantError: true, wantEvents: "scope-start,start,wait,scope-finish"},
		{name: "finish-failure", finishErr: errors.New("private-finish-failure"), wantError: true, wantEvents: "scope-start,start,wait,scope-finish"},
	} {
		t.Run(test.name, func(t *testing.T) { runNativeCaptureLifetimeCase(t, test) })
	}
}

func runNativeCaptureLifetimeCase(t *testing.T, test nativeCaptureLifetimeCase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var scopeCommand, startedCommand, waitedCommand, finishedCommand *exec.Cmd
	var events []string
	scope := nativeTestScope{
		ctx:       ctx,
		authorize: func() error { return nil },
		startCommand: func(cmd *exec.Cmd, start func() error) error {
			events = append(events, "scope-start")
			scopeCommand = cmd
			return start()
		},
		finishCommand: func(cmd *exec.Cmd) error {
			events = append(events, "scope-finish")
			finishedCommand = cmd
			return test.finishErr
		},
	}
	data, err := runNative(scope, captureDefinition(t), nativeHooks{
		start: func(cmd *exec.Cmd) error {
			events = append(events, "start")
			startedCommand = cmd
			return test.startErr
		},
		wait: func(cmd *exec.Cmd) error {
			events = append(events, "wait")
			waitedCommand = cmd
			return test.waitErr
		},
	})
	defer clear(data)
	assertNativeCaptureLifetimeCommands(t, test, events, scopeCommand, startedCommand, waitedCommand, finishedCommand)
	assertNativeCaptureLifetimeResult(t, test, data, err)
}

func assertNativeCaptureLifetimeCommands(t *testing.T, test nativeCaptureLifetimeCase, events []string, scopeCommand, startedCommand, waitedCommand, finishedCommand *exec.Cmd) {
	t.Helper()
	if got := strings.Join(events, ","); got != test.wantEvents {
		t.Fatalf("command lifetime order = %q, want %q", got, test.wantEvents)
	}
	if scopeCommand == nil || scopeCommand != startedCommand {
		t.Fatal("StartCommand did not wrap the exact started command")
	}
	if test.startErr != nil {
		if waitedCommand != nil || finishedCommand != nil {
			t.Fatal("failed start was waited or finished")
		}
		return
	}
	if waitedCommand != scopeCommand || finishedCommand != scopeCommand {
		t.Fatal("Wait and FinishCommand did not receive the exact started command")
	}
}

func assertNativeCaptureLifetimeResult(t *testing.T, test nativeCaptureLifetimeCase, data []byte, err error) {
	t.Helper()
	if !test.wantError {
		if err != nil {
			t.Fatalf("successful command lifetime failed: %v", err)
		}
		return
	}
	if data != nil || !errors.Is(err, errNativeInspection) || strings.Contains(err.Error(), "private-") {
		t.Fatal("lifetime failure exposed diagnostics or bytes")
	}
}

func TestNativeCaptureRejectsAndDrainsOverflow(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			waits := 0
			data, err := runNative(nativeTestScope{ctx: ctx, authorize: func() error { return nil }}, captureDefinition(t), nativeHooks{
				start: func(cmd *exec.Cmd) error {
					writer := cmd.Stdout
					if stream == "stderr" {
						writer = cmd.Stderr
					}
					// Exceeds pipe capacity: capture must keep draining after its cap.
					_, writeErr := io.WriteString(writer, strings.Repeat("s", 256*1024))
					return writeErr
				},
				wait: func(*exec.Cmd) error { waits++; return nil },
			})
			if data != nil || !errors.Is(err, errNativeInspection) || waits != 1 {
				t.Fatalf("overflow must reject only after owned wait: %v waits=%d", err, waits)
			}
		})
	}
}

func TestNativeCaptureFailureDoesNotExposeErrors(t *testing.T) {
	for _, phase := range []string{"gate", "start", "wait"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			starts, waits := 0, 0
			fail := func(at string) error {
				if phase == at {
					return fmt.Errorf("private-sentinel-%s", at)
				}
				return nil
			}
			data, err := runNative(nativeTestScope{ctx: ctx, authorize: func() error { return fail("gate") }}, captureDefinition(t), nativeHooks{
				start: func(*exec.Cmd) error { starts++; return fail("start") },
				wait:  func(*exec.Cmd) error { waits++; return fail("wait") },
			})
			if data != nil || !errors.Is(err, errNativeInspection) || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("native failure exposed diagnostics or bytes")
			}
			if (phase == "gate" && starts != 0) || (phase != "wait" && waits != 0) || (phase == "wait" && waits != 1) {
				t.Fatalf("wrong operation ownership starts=%d waits=%d", starts, waits)
			}
		})
	}
}

func TestNativeCaptureCancellationAfterStartStillWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	waited := false
	data, err := runNative(nativeTestScope{ctx: ctx, authorize: func() error { return nil }}, captureDefinition(t), nativeHooks{
		start: func(cmd *exec.Cmd) error {
			cancel()
			_, writeErr := io.WriteString(cmd.Stdout, "secret")
			return writeErr
		},
		wait: func(*exec.Cmd) error { waited = true; return nil },
	})
	if !waited || data != nil || !errors.Is(err, errNativeInspection) {
		t.Fatal("cancellation abandoned wait or accepted late data")
	}
}

func TestNativeCaptureRequiresDeadlineAndGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		gate func() error
	}{
		{"missing-deadline", context.Background(), func() error { return nil }},
		{"missing-gate", ctx, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := runNative(nativeTestScope{ctx: test.ctx, authorize: test.gate}, captureDefinition(t), nativeHooks{
				start: func(*exec.Cmd) error { t.Fatal("unauthorized start"); return nil },
			})
			if data != nil || !errors.Is(err, errNativeInspection) {
				t.Fatal("missing lifetime accepted")
			}
		})
	}
}

func TestBoundedNativeBufferBoundaryAndDiscard(t *testing.T) {
	for _, retain := range []bool{false, true} {
		buffer := boundedNativeBuffer{remaining: 3, retain: retain}
		if n, err := buffer.Write([]byte("abc")); n != 3 || err != nil || buffer.overflow {
			t.Fatal("exact cap rejected")
		}
		if n, err := buffer.Write([]byte("d")); n != 1 || err != nil || !buffer.overflow {
			t.Fatal("overflow not drained")
		}
		if (retain && string(buffer.bytes) != "abc") || (!retain && len(buffer.bytes) != 0) {
			t.Fatal("retention bound violated")
		}
		clear(buffer.bytes)
	}
}
