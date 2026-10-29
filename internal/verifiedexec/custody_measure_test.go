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
	"syscall"
	"testing"
	"time"

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

// usage is the storage a set of trees holds. Named counts every name as its own
// file; unique counts each inode once; allocated is st_blocks * 512 of unique inodes.
type usage struct {
	NamedLogical  uint64
	UniqueLogical uint64
	Allocated     uint64
	Inodes        int
}

type inodeKey struct{ dev, ino uint64 }

func measureTrees(roots ...string) (usage, error) {
	var total usage
	seen := map[inodeKey]bool{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || !entry.Type().IsRegular() {
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("no stat for %s", path)
			}
			total.NamedLogical += uint64(stat.Size)
			key := inodeKey{dev: uint64(stat.Dev), ino: stat.Ino}
			if !seen[key] {
				seen[key] = true
				total.UniqueLogical += uint64(stat.Size)
				total.Allocated += uint64(stat.Blocks) * 512
				total.Inodes++
			}
			return nil
		})
		if err != nil {
			return usage{}, err
		}
	}
	return total, nil
}

// openDescriptors counts this process's open descriptors, excluding the one the
// listing itself uses.
func openDescriptors() (int, error) {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0, err
	}
	return len(entries) - 1, nil
}

func descriptorLimit() (uint64, error) {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return 0, err
	}
	return limit.Cur, nil
}

// childResources is what the kernel reported for one waited child.
type childResources struct {
	CPU         time.Duration
	MaxRSSBytes int64
}

func resourcesOf(state *os.ProcessState) (childResources, bool) {
	rusage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok {
		return childResources{}, false
	}
	rss := rusage.Maxrss
	if runtime.GOOS == "linux" {
		rss *= 1024 // Linux reports kilobytes; Darwin reports bytes.
	}
	return childResources{CPU: state.UserTime() + state.SystemTime(), MaxRSSBytes: rss}, true
}

// listXattrs returns the extended attribute names on path.
func listXattrs(path string) (string, error) {
	buffer := make([]byte, 4096)
	size, err := unix.Listxattr(path, buffer)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.TrimRight(string(buffer[:size]), "\x00"), "\x00", ","), nil
}
