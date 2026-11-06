package goroutinecheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestRunPreservesFailuresAndChecksAfterTests(t *testing.T) {
	leak := errors.New("fixture goroutine remained")
	for _, test := range []struct {
		name string
		code int
		leak error
		want int
	}{
		{name: "clean success", want: 0},
		{name: "clean failure", code: 7, want: 7},
		{name: "leaking success", leak: leak, want: 1},
		{name: "leaking failure", code: 7, leak: leak, want: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			finished := false
			checked := false
			code := run(func() int {
				finished = true
				return test.code
			}, func() error {
				if !finished {
					t.Fatal("leak check ran before tests completed")
				}
				checked = true
				return test.leak
			}, &output)
			if code != test.want || !checked {
				t.Fatalf("exit=%d checked=%t; want exit=%d and a leak check", code, checked, test.want)
			}
			if test.leak == nil && output.Len() != 0 {
				t.Fatalf("clean suite emitted %q", output.String())
			}
			if test.leak != nil && !strings.Contains(output.String(), test.leak.Error()) {
				t.Fatalf("leak diagnostic missing from %q", output.String())
			}
		})
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRunDiagnosticFailureKeepsFailingExit(t *testing.T) {
	for _, initial := range []int{0, 7} {
		got := run(func() int { return initial }, func() error { return errors.New("leak") }, failedWriter{})
		want := initial
		if want == 0 {
			want = 1
		}
		if got != want {
			t.Fatalf("initial=%d exit=%d want=%d", initial, got, want)
		}
	}
}

func TestGoleakFindDetectsOwnedGoroutineAndAcceptsClosure(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	cleanup := func() {
		once.Do(func() { close(release) })
		<-done
	}
	t.Cleanup(cleanup)
	go func() {
		defer close(done)
		pprof.Do(context.Background(), pprof.Labels("owner", "goroutinecheck-test"), func(context.Context) {
			close(started)
			<-release
		})
	}()
	<-started
	if err := goleak.Find(); err == nil || !strings.Contains(err.Error(), "goroutinecheck.TestGoleakFindDetectsOwnedGoroutineAndAcceptsClosure") {
		t.Fatalf("expected the running owned goroutine to be detected: %v", err)
	}
	cleanup()
	if err := goleak.Find(); err != nil {
		t.Fatalf("joined goroutine still reported: %v", err)
	}
}
