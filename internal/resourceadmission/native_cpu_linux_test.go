//go:build linux

package resourceadmission

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const realisticStat = "cpu  4705 150 1120 16250 520 30 45 10 99 7\ncpu0 1 2 3 4 5 6 7 8 9 10\n"

func TestParseProcStatCPUSumsExclusiveStatesAndExcludesGuest(t *testing.T) {
	got, err := parseProcStatCPU("cpu  4705 150 1120 16250 520 30 45 10 99 7")
	if err != nil {
		t.Fatal(err)
	}
	// total = 4705+150+1120+16250+520+30+45+10; busy drops idle and iowait.
	want := cpuCounters{busy: 4705 + 150 + 1120 + 30 + 45 + 10, total: 22830}
	if got != want {
		t.Fatalf("counters = %+v, want %+v", got, want)
	}
}

func TestParseProcStatCPUAcceptsOldKernelsWithFourFields(t *testing.T) {
	got, err := parseProcStatCPU("cpu 10 0 5 85")
	if err != nil || got != (cpuCounters{busy: 15, total: 100}) {
		t.Fatalf("counters = %+v err = %v", got, err)
	}
}

func TestParseProcStatCPURejectsInvalidLines(t *testing.T) {
	cases := map[string]string{
		"per-cpu line":     "cpu0 1 2 3 4 5 6 7 8",
		"empty":            "",
		"too few fields":   "cpu 1 2 3",
		"missing fields":   "cpu",
		"non numeric":      "cpu 1 2 x 4",
		"negative":         "cpu 1 -2 3 4",
		"fractional":       "cpu 1 2.5 3 4",
		"hex":              "cpu 0x1 2 3 4",
		"bad extra field":  "cpu 1 2 3 4 5 6 7 8 9 z",
		"zero total":       "cpu 0 0 0 0 0 0 0 0",
		"overflow":         "cpu 18446744073709551615 1 0 0",
		"beyond uint64":    "cpu 18446744073709551616 1 1 1",
		"other prefix":     "cpux 1 2 3 4",
		"wrong first word": "intr 1 2 3 4 5",
	}
	for name, line := range cases {
		if _, err := parseProcStatCPU(line); !errors.Is(err, errProcStat) {
			t.Errorf("%s: err = %v, want a malformed aggregate error", name, err)
		}
	}
}

func TestReadProcStatCPUReadsOnlyTheFirstLine(t *testing.T) {
	got := readProcStatCPU(strings.NewReader(realisticStat))
	if !got.ok || got.value.total != 22830 {
		t.Fatalf("reading = %+v", got)
	}
}

type countingReader struct {
	remaining int
	read      int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	c.remaining -= n
	c.read += n
	return n, nil
}

func TestReadProcStatCPUIsBounded(t *testing.T) {
	r := &countingReader{remaining: 1 << 30}
	got := readProcStatCPU(r)
	if got.ok || got.failure() != ReasonMalformed {
		t.Fatalf("reading = %+v, want malformed for an unterminated huge line", got)
	}
	if r.read > procStatLimit {
		t.Fatalf("read %d bytes, want at most %d", r.read, procStatLimit)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadProcStatCPUFailuresAreUnavailableNotZero(t *testing.T) {
	if got := readProcStatCPU(failingReader{}); got.ok || got.failure() != ReasonUnreadable {
		t.Fatalf("reading = %+v, want unreadable", got)
	}
	if got := readProcStatCPU(strings.NewReader("cpu 1 2 3 4")); got.ok {
		t.Fatalf("reading = %+v, want malformed without a newline", got)
	}
	if got := readProcStatCPU(strings.NewReader("cpu 0 0 0 0\n")); got.ok {
		t.Fatalf("reading = %+v, want unavailable for a zero total", got)
	}
}

func TestProcStatCounterRegressionIsUnknownThroughPolicy(t *testing.T) {
	first := readProcStatCPU(strings.NewReader("cpu 100 0 100 800 0 0 0 0\n"))
	second := readProcStatCPU(strings.NewReader("cpu 50 0 50 800 0 0 0 0\n"))
	if got := hostCPUOutcome(first, second); got.reason != ReasonCounterReset {
		t.Fatalf("outcome = %+v, want counter_reset", got)
	}
	busier := readProcStatCPU(strings.NewReader("cpu 195 0 100 805 0 0 0 0\n"))
	if got := hostCPUOutcome(first, busier); got.state != outcomeHot {
		t.Fatalf("outcome = %+v, want hot at 195/200 busy", got)
	}
}

func TestReadHostCPUIgnoresInheritedHostProc(t *testing.T) {
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "stat"), []byte("cpu 1 0 1 1 0 0 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_PROC", fake)
	t.Setenv("HOST_SYS", fake)
	got := readHostCPU(context.Background())
	if !got.ok {
		t.Fatalf("host cpu unavailable: %s", got.failure())
	}
	if got.value.total <= 3 {
		t.Fatalf("total = %v, want the real /proc/stat and not the inherited fake host", got.value.total)
	}
}

func TestReadHostCPUAdvances(t *testing.T) {
	first := readHostCPU(context.Background())
	second := readHostCPU(context.Background())
	if !first.ok || !second.ok {
		t.Fatalf("host cpu unavailable: %s %s", first.failure(), second.failure())
	}
	if second.value.total < first.value.total || second.value.busy < first.value.busy {
		t.Fatalf("counters went backwards: %+v then %+v", first.value, second.value)
	}
}
