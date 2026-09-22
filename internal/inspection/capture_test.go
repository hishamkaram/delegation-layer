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
	ctx       context.Context
	authorize func() error
}

func (s nativeTestScope) Context() context.Context { return s.ctx }

func (s nativeTestScope) Start(start func() error) error {
	if s.ctx == nil || s.ctx.Err() != nil || s.authorize == nil {
		return errNativeInspection
	}
	if err := s.authorize(); err != nil {
		return err
	}
	return start()
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
