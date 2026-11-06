package verifiedexec

// Experimental F19 custody proof: receipts and resource measurement. Hard-link
// counts, allocated blocks and descriptor counts are metadata facts about this
// run; they are not claims about shared RAM (PSS) or other hosts.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type proofObservation struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type proofReceipt struct {
	Test         string             `json:"test"`
	Host         string             `json:"host"`
	Kernel       string             `json:"kernel"`
	Observations []proofObservation `json:"observations"`
}

// recorder collects the observations of one test and writes them as a receipt
// when the operator named a receipt directory.
type recorder struct {
	t       *testing.T
	receipt proofReceipt
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{t: t, receipt: proofReceipt{Test: t.Name(), Host: runtime.GOOS + "/" + runtime.GOARCH, Kernel: kernelFacts()}}
	t.Cleanup(r.write)
	return r
}

func (r *recorder) note(name, format string, args ...any) {
	value := fmt.Sprintf(format, args...)
	r.receipt.Observations = append(r.receipt.Observations, proofObservation{Name: name, Value: value})
	r.t.Logf("observed %s: %s", name, value)
}

func (r *recorder) write() {
	dir := os.Getenv(proofReceiptEnv)
	if dir == "" {
		return
	}
	content, err := json.MarshalIndent(r.receipt, "", "  ")
	if err != nil {
		r.t.Errorf("encode receipt: %v", err)
		return
	}
	name := strings.ReplaceAll(r.t.Name(), "/", "__") + ".json"
	if err = os.WriteFile(filepath.Join(dir, name), append(content, '\n'), 0o600); err != nil {
		r.t.Errorf("write receipt: %v", err)
	}
}

func kernelFacts() string {
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return "uname failed: " + err.Error()
	}
	return cString(name.Sysname[:]) + " " + cString(name.Release[:]) + " " + cString(name.Machine[:])
}

func cString(raw []byte) string {
	if end := strings.IndexByte(string(raw), 0); end >= 0 {
		return string(raw[:end])
	}
	return string(raw)
}
