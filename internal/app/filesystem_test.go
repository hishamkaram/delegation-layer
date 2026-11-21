package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspacefiles"
)

type filesystemCLIFixture struct {
	root    string
	path    string
	request workspacefiles.Request
}

func newFilesystemCLIFixture(t *testing.T) filesystemCLIFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "native")
	store, err := taskdir.InitStoreWithPredicatesContext(t.Context(), root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	token, err := store.RootTokenContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "workspace")
	if err = os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := taskdir.ObservePathProjectContext(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := taskdir.ProjectToken(identity)
	if err != nil {
		t.Fatal(err)
	}
	return filesystemCLIFixture{root: root, path: path, request: workspacefiles.Request{ExpectedRoot: token, Scope: workspacefiles.Scope{Kind: workspacefiles.Workspace, ExpectedWorkspace: project}}}
}

func runFilesystemCLI(t *testing.T, f filesystemCLIFixture, operation string, request workspacefiles.Request) (filesystemEnvelope, int) {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"filesystem", operation, "--root", f.root, "--request", "-", "--json"}, &stdout, &stderr, Dependencies{Input: bytes.NewReader(data)})
	var envelope filesystemEnvelope
	if err = json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("exit=%d stdout=%q stderr=%q: %v", code, &stdout, &stderr, err)
	}
	return envelope, code
}

func TestFilesystemPublicCommandsPreserveRootAndExactBytes(t *testing.T) {
	f := newFilesystemCLIFixture(t)
	name := "hidden\\name:\x01"
	data := []byte{0xff, 0, 'x', '\n'}
	writeAppTestFile(t, filepath.Join(f.path, name), data)
	before := snapshotRebootTaskFiles(t, f.root)
	page, code := runFilesystemCLI(t, f, "list", f.request)
	if code != 0 || page.Filesystem == nil || page.Filesystem.List == nil || len(page.Filesystem.List.Entries) != 1 {
		t.Fatalf("list: code=%d %+v", code, page)
	}
	request := f.request
	request.RelativePathBase64 = page.Filesystem.List.Entries[0].RelativePathBase64
	read, code := runFilesystemCLI(t, f, "read", request)
	if code != 0 || read.Filesystem == nil || read.Filesystem.Read == nil || read.Filesystem.Read.DataBase64 != base64.StdEncoding.EncodeToString(data) || read.Filesystem.Read.Text != nil {
		t.Fatalf("read: code=%d %+v", code, read)
	}
	for _, operation := range []string{"inspect", "git", "scopes"} {
		envelope, operationCode := runFilesystemCLI(t, f, operation, f.request)
		if operationCode != 0 || envelope.Filesystem == nil || envelope.Filesystem.Operation != workspacefiles.Operation(operation) {
			t.Fatalf("%s: code=%d %+v", operation, operationCode, envelope)
		}
	}
	if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, f.root)) {
		t.Fatal("public browsing changed Native state or created tasks")
	}
}

func TestFilesystemPureScopesDoesNotResolveOrCreateDefaultRoot(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"filesystem", "scopes", "--request", "-", "--json"}, &stdout, &stderr, Dependencies{Input: strings.NewReader("{}")})
	var envelope filesystemEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("exit=%d stdout=%q stderr=%q: %v", code, &stdout, &stderr, err)
	}
	if code != 0 || envelope.Filesystem == nil || envelope.Filesystem.Scopes == nil || len(envelope.Filesystem.Scopes.Scopes) == 0 {
		t.Fatalf("OS scope discovery required a Native root: code=%d %+v", code, envelope)
	}
	if runtime.GOOS == "linux" && envelope.Filesystem.Scopes.Scopes[0].Kind != workspacefiles.SharedTemp {
		t.Fatal("Linux did not advertise shared temp")
	}
}

func TestFilesystemRequestFileIsStrictBoundedAndRegular(t *testing.T) {
	f := newFilesystemCLIFixture(t)
	before := snapshotRebootTaskFiles(t, f.root)
	for _, contents := range []string{"{", `{"unexpected":"private sentinel"}`, strings.Repeat(" ", task.MaxControlRecordSize+1)} {
		path := filepath.Join(t.TempDir(), "request.json")
		writeAppTestFile(t, path, []byte(contents))
		assertFilesystemRequestRejected(t, f.root, path)
	}
	pipe := filepath.Join(t.TempDir(), "request.fifo")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	assertFilesystemRequestRejected(t, f.root, pipe)
	assertFilesystemRequestRejected(t, f.root, t.TempDir())
	if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, f.root)) {
		t.Fatal("invalid filesystem request changed Native state")
	}
}

func assertFilesystemRequestRejected(t *testing.T, root, path string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"filesystem", "list", "--root", root, "--request", path, "--json"}, &stdout, &stderr, Dependencies{})
	var envelope filesystemEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if code != 2 || envelope.Filesystem != nil || envelope.Failure == nil || envelope.Failure.Code != workspacefiles.InvalidRequest {
		t.Fatalf("invalid request exit=%d %+v", code, envelope)
	}
	if strings.Contains(stdout.String()+stderr.String(), path) || strings.Contains(stdout.String()+stderr.String(), "private sentinel") {
		t.Fatal("failure leaked request data or path")
	}
}

func TestFilesystemDeadlineWithholdsContentAndKeepsStdinOwnedByCaller(t *testing.T) {
	f := newFilesystemCLIFixture(t)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(read.Close(), write.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := RunContext(ctx, []string{"filesystem", "read", "--root", f.root, "--request", "-", "--json"}, &stdout, &stderr, Dependencies{Input: read})
	var envelope filesystemEnvelope
	if err = json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if code == 0 || envelope.Filesystem != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("deadline released data: exit=%d %+v", code, envelope)
	}
	if envelope.Failure == nil || envelope.Failure.Code != workspacefiles.Timeout || !envelope.Failure.Retryable {
		t.Fatalf("deadline was classified as a permanent invalid request: %+v", envelope)
	}
	if _, err = read.Stat(); err != nil {
		t.Fatal("command closed borrowed stdin")
	}
}

type filesystemFailingWriter struct{}

func (filesystemFailingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestFilesystemOutputFailureIsReportedWithoutStateChanges(t *testing.T) {
	f := newFilesystemCLIFixture(t)
	before := snapshotRebootTaskFiles(t, f.root)
	data, err := json.Marshal(f.request)
	if err != nil {
		t.Fatal(err)
	}
	code := Run([]string{"filesystem", "list", "--root", f.root, "--request", "-", "--json"}, filesystemFailingWriter{}, io.Discard, Dependencies{Input: bytes.NewReader(data)})
	if code != 1 || !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, f.root)) {
		t.Fatal("output failure succeeded or mutated Native root")
	}
}

func TestPublicContextProjectsExecutableFeaturesWithoutRootMutation(t *testing.T) {
	f := newFilesystemCLIFixture(t)
	before := snapshotRebootTaskFiles(t, f.root)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"context", "inspect", "--root", f.root, "--json"}, &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("exit=%d stdout=%q stderr=%q: %v", code, &stdout, &stderr, err)
	}
	if code != 0 || response.Connector == nil {
		t.Fatalf("context inspection failed: code=%d %+v", code, response)
	}
	for _, feature := range []string{"workspace-filesystem-v1", "native-execution-options-v1", "project-token-sha256-v1", "guarded-continuation-v1", task.OriginBindingFeature} {
		if !slices.Contains(response.Connector.Features, feature) {
			t.Fatalf("missing executable capability %s", feature)
		}
	}
	if !reflect.DeepEqual(before, snapshotRebootTaskFiles(t, f.root)) {
		t.Fatal("executable feature projection rewrote root requirements")
	}
}
