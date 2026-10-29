package verifiedexec

// Fixtures for the executable-custody tests: typed child reports, owned
// executables, mutations, and handshake children whose every pipe endpoint is
// owned and closed on every path.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	scaleEnableEnv     = "DELEGATE_CUSTODY_PROOF"
	proofReceiptEnv    = "DELEGATE_CUSTODY_PROOF_RECEIPT_DIR"
	proofScratchEnv    = "DELEGATE_CUSTODY_PROOF_SCRATCH"
	proofOtherFSEnv    = "DELEGATE_CUSTODY_PROOF_OTHER_FS_DIR"
	proofLargeBytesEnv = "DELEGATE_CUSTODY_PROOF_LARGE_BYTES"
)

// proofReport is the single JSON line an owned fixture child prints.
type proofReport struct {
	Marker        string `json:"marker"`
	Executable    string `json:"executable,omitempty"`
	Argv0         string `json:"argv0,omitempty"`
	ImportMetaURL string `json:"import_meta_url,omitempty"`
	Dependency    string `json:"dependency,omitempty"`
	Parent        string `json:"parent,omitempty"`
	Nested        string `json:"nested,omitempty"`
	Resource      string `json:"resource,omitempty"`
	Runtime       string `json:"runtime,omitempty"`
	Handshake     string `json:"handshake,omitempty"`
}

func parseReport(output []byte) (proofReport, error) {
	for _, line := range bytes.Split(output, []byte("\n")) {
		if len(line) == 0 || bytes.Equal(line, []byte("READY")) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var report proofReport
		if err := decoder.Decode(&report); err != nil {
			return proofReport{}, fmt.Errorf("decode child report %q: %w", line, err)
		}
		return report, nil
	}
	return proofReport{}, errors.New("child printed no report")
}

// requireScale skips the scale tests (hundreds of children, hundreds of MB)
// unless the operator asked for them.
func requireScale(t *testing.T) {
	t.Helper()
	if os.Getenv(scaleEnableEnv) != "1" {
		t.Skipf("scale proof not requested: set %s=1", scaleEnableEnv)
	}
}

// scratch returns a test-owned directory below the operator's observed native
// proof directory when given, else below the native temporary root. Views are
// sealed read-only, so removal unseals them first.
func scratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv(proofScratchEnv), "custody-")
	if err != nil {
		t.Fatalf("setup blocked: scratch directory: %v", err)
	}
	t.Cleanup(func() {
		if unsealErr := unsealTemporaryDirectory(dir); unsealErr != nil {
			t.Errorf("unseal scratch: %v", unsealErr)
		}
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			t.Errorf("remove scratch: %v", removeErr)
		}
	})
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("canonical scratch: %v", err)
	}
	if err = os.Chmod(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	return canonical
}

func writeExecutable(t *testing.T, path string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}
	return digestOf(content)
}

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// selfNative copies this test binary to dir as an owned native executable and
// returns its path and digest. Run with helperArg it is the tiny native helper.
func selfNative(t *testing.T, dir string) (path, digest string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "entry-native")
	copyFileTo(t, self, path)
	return path, fileDigest(t, path)
}

// runToCompletion starts the bound child, collects stdout and waits for it.
func runToCompletion(command *Command) (proofReport, error) {
	var stdout, stderr bytes.Buffer
	command.Cmd.Stdout, command.Cmd.Stderr = &stdout, &stderr
	if err := command.Cmd.Run(); err != nil {
		return proofReport{}, fmt.Errorf("run bound child: %w (stderr %q)", err, stderr.String())
	}
	return parseReport(stdout.Bytes())
}

func closeFiles(files ...*os.File) error {
	var err error
	for _, file := range files {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}
	return err
}

// liveChild is a started child blocked on the READY/GO handshake. It owns both
// parent-side pipe endpoints; the child-side endpoints are closed right after
// Start.
type liveChild struct {
	cmd      *exec.Cmd
	stdin    *os.File
	outRead  *os.File
	stdout   *bufio.Reader
	finished bool
	result   error
}

var errStdioConfigured = errors.New("handshake child: stdin or stdout is already configured")

// startHandshake starts the child with pipes it owns outright. Every endpoint
// it creates is closed on every failure path before Start; once the child is
// started a cleanup closes its input and Waits if the test leaves before
// release does.
func startHandshake(t *testing.T, command *Command) (*liveChild, error) {
	t.Helper()
	cmd := command.Cmd
	if cmd.Stdin != nil || cmd.Stdout != nil {
		return nil, errStdioConfigured
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, closeFiles(inRead, inWrite))
	}
	cmd.Stdin, cmd.Stdout = inRead, outWrite
	if err = cmd.Start(); err != nil {
		return nil, errors.Join(err, closeFiles(inRead, inWrite, outRead, outWrite))
	}
	child := &liveChild{cmd: cmd, stdin: inWrite, outRead: outRead, stdout: bufio.NewReader(outRead)}
	t.Cleanup(func() {
		if !child.finished {
			if finishErr := child.finish(); finishErr != nil {
				t.Errorf("cleanup of an unreleased handshake child: %v", finishErr)
			}
		}
	})
	if err = closeFiles(inRead, outWrite); err != nil {
		return nil, errors.Join(err, child.finish())
	}
	line, err := child.stdout.ReadString('\n')
	if err != nil || line != "READY\n" {
		return nil, errors.Join(fmt.Errorf("child did not announce READY: %q: %w", line, err), child.finish())
	}
	return child, nil
}

// finish closes the handshake input and Waits once; later calls repeat the
// first result.
func (c *liveChild) finish() error {
	if c.finished {
		return c.result
	}
	c.finished = true
	closeErr := c.stdin.Close()
	waitErr := c.cmd.Wait()
	c.result = errors.Join(closeErr, waitErr, c.outRead.Close())
	return c.result
}

// release answers GO, reads the report and waits for the child.
func (c *liveChild) release() (proofReport, error) {
	if _, err := io.WriteString(c.stdin, "GO\n"); err != nil {
		return proofReport{}, errors.Join(err, c.finish())
	}
	rest, readErr := io.ReadAll(c.stdout)
	report, parseErr := parseReport(rest)
	return report, errors.Join(readErr, c.finish(), parseErr)
}

// shellFixture is an owned #!<shell> script; marker is exactly three bytes.
func shellFixture(shell, marker string, handshake bool) []byte {
	body := "#!" + shell + "\n"
	if handshake {
		body += "printf 'READY\\n'\nread line\n"
		return []byte(body + "printf '{\"marker\":\"" + marker + "\",\"argv0\":\"%s\",\"handshake\":\"%s\"}\\n' \"$0\" \"$line\"\n")
	}
	return []byte(body + "printf '{\"marker\":\"" + marker + "\",\"argv0\":\"%s\"}\\n' \"$0\"\n")
}

const nodeEntryTemplate = `#!/usr/bin/env node
import { dependency } from "./dep.mjs";
import { parent } from "../parent.mjs";
import { nested } from "./nested/deep.mjs";
import { readFileSync } from "node:fs";
const resource = readFileSync(new URL("./data.txt", import.meta.url), "utf8").trim();
process.stdout.write(JSON.stringify({ marker: "MARK", argv0: process.argv[1], import_meta_url: import.meta.url, dependency, parent, nested, resource, runtime: process.version }) + "\n");
`

func nodeFixture(marker string) []byte {
	return []byte(strings.Replace(nodeEntryTemplate, "MARK", marker, 1))
}

// nodeTree writes the entry and its relative-import neighbours below dir/bin.
func nodeTree(t *testing.T, dir, marker string) (entry string) {
	t.Helper()
	files := map[string]string{
		"bin/entry.js":         string(nodeFixture(marker)),
		"bin/dep.mjs":          "export const dependency = \"DEP\";\n",
		"bin/data.txt":         "RES\n",
		"bin/nested/deep.mjs":  "export const nested = \"NEST\";\n",
		"parent.mjs":           "export const parent = \"PAR\";\n",
		"package.json":         "{\"type\":\"module\"}\n",
		"bin/package.json":     "{\"type\":\"module\"}\n",
		"bin/nested/extra.txt": "x\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "bin", "entry.js")
}

// discoverPath finds a tool on this command's own PATH, canonicalized, and
// skips the test when the host does not have it.
func discoverPath(t *testing.T, name string) string {
	t.Helper()
	found, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH: %v", name, err)
	}
	canonical, err := filepath.EvalSymlinks(found)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// pathFor is the PATH a fixture launch uses: the directories of the tools it
// needs, discovered from this command's own PATH.
func pathFor(tools ...string) []string {
	var dirs []string
	for _, tool := range tools {
		if found, err := exec.LookPath(tool); err == nil {
			dirs = append(dirs, filepath.Dir(found))
		}
	}
	return []string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator))}
}

type mutation struct {
	name  string
	apply func(path string, replacement []byte) error
}

// mutations are the three ways an installer or update changes a path after
// bind. Each must leave the original visibly different.
var mutations = []mutation{
	{"rename-over", func(path string, replacement []byte) error {
		if err := os.Rename(path, path+".moved"); err != nil {
			return err
		}
		return os.WriteFile(path, replacement, 0o700)
	}},
	{"in-place", func(path string, replacement []byte) error {
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteAt(replacement, 0)
		return errors.Join(writeErr, file.Close())
	}},
	{"truncate", func(path string, _ []byte) error { return os.Truncate(path, 0) }},
}

// mutateAndConfirm applies m and proves the original no longer has wantDigest.
func mutateAndConfirm(t *testing.T, m mutation, path string, replacement []byte, wantDigest string) {
	t.Helper()
	if err := m.apply(path, replacement); err != nil {
		t.Fatalf("%s mutation of the original failed: %v", m.name, err)
	}
	if fileDigest(t, path) == wantDigest {
		t.Fatalf("%s mutation left the original unchanged; the control is void", m.name)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	if err = errors.Join(copyErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func deviceOf(t *testing.T, info os.FileInfo) uint64 {
	t.Helper()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no device")
	}
	return uint64(stat.Dev)
}

// copyFileTo copies src to dst as an owned, executable file.
func copyFileTo(t *testing.T, src, dst string) {
	t.Helper()
	input, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := input.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(output, input); err != nil {
		t.Fatal(err)
	}
	if err = output.Close(); err != nil {
		t.Fatal(err)
	}
}

var errSetupControl = errors.New("setup control failed: the owned interpreter does not run on its own, before any custody")

// directInterpreterControl is the positive control for an owned interpreter: it
// must run directly, with no custody, before it is bound. A failure here is a
// fixture defect (for example a lost @rpath neighbour), never a custody result.
func directInterpreterControl(path string, args ...string) error {
	output, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		if len(output) > 400 {
			output = output[:400]
		}
		return fmt.Errorf("%w: %s %v: %w (output %q)", errSetupControl, path, args, err, output)
	}
	return nil
}

// ownedInterpreterCopy copies source into root at the position it has in its
// own installation, using the builder's shadow-directory layout: the file is a
// regular owned copy, and every neighbour and ancestor entry is a symlink to
// the original installation. Loader paths such as @loader_path/../lib therefore
// resolve exactly as they do for the original, with no DYLD or LD override.
// Only the copied file is ever mutated by a test; the symlinks are never
// written through.
func ownedInterpreterCopy(t *testing.T, root, source string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mirror, err := shadowDirectory(filepath.Dir(source), root, filepath.Base(source), root)
	if err != nil {
		t.Fatalf("setup blocked: lay out %s beside its installation: %v", source, err)
	}
	owned := filepath.Join(mirror, filepath.Base(source))
	copyFileTo(t, source, owned)
	return owned
}
