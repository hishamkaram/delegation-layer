package workspacefiles

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type fixture struct {
	root    string
	path    string
	request Request
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	store, err := taskdir.InitStoreWithPredicatesContext(t.Context(), filepath.Join(base, "native"), predicate.Default())
	must(t, err)
	rootToken, err := store.RootTokenContext(t.Context())
	must(t, err)
	must(t, store.Close())
	path := filepath.Join(base, "workspace")
	must(t, os.Mkdir(path, 0o700))
	identity, err := taskdir.ObservePathProjectContext(t.Context(), path)
	must(t, err)
	token, err := taskdir.ProjectToken(identity)
	must(t, err)
	return fixture{
		root: filepath.Join(base, "native"), path: path,
		request: Request{ExpectedRoot: rootToken, Scope: Scope{Kind: Workspace, Directory: identity.Path, ExpectedWorkspace: token}},
	}
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	must(t, os.WriteFile(path, data, 0o600))
}

func execute(t *testing.T, f fixture, operation Operation, request Request) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := Execute(ctx, f.root, operation, request)
	must(t, err)
	if result.Operation != operation || result.SchemaVersion != 1 || result.ObservedAt.IsZero() {
		t.Fatalf("invalid observation envelope: %+v", result)
	}
	return result
}

func requireCode(t *testing.T, err error, expected ErrorCode) {
	t.Helper()
	var observed *Error
	if !errors.As(err, &observed) || observed.Code != expected {
		t.Fatalf("error = %v, want code %s", err, expected)
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	must(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += string(data)
		}
		result[path] = value
		return nil
	}))
	return result
}

func TestPublicReadsPreserveNativeStateAndExactRanges(t *testing.T) {
	f := newFixture(t)
	data := append(bytes.Repeat([]byte("x"), MaxReadBytes), 0xff, 0, 'z')
	write(t, filepath.Join(f.path, ".hidden: binary"), data)
	must(t, os.Link(filepath.Join(f.path, ".hidden: binary"), filepath.Join(f.path, "hardlink")))
	before := snapshot(t, f.root)
	request := f.request
	request.RelativePath = ".hidden: binary"
	first := execute(t, f, Read, request).Read
	if first == nil || first.EOF || first.NextOffset != MaxReadBytes || first.Text == nil {
		t.Fatalf("first chunk: %+v", first)
	}
	request.Offset = first.NextOffset
	request.ExpectedVersion = first.Version
	last := execute(t, f, Read, request).Read
	decoded, err := base64.StdEncoding.DecodeString(last.DataBase64)
	must(t, err)
	if !bytes.Equal(decoded, []byte{0xff, 0, 'z'}) || last.Text != nil || !last.EOF {
		t.Fatalf("binary tail: %+v decoded %v", last, decoded)
	}
	request.RelativePath, request.Offset = "hardlink", 0
	hardlink := execute(t, f, Read, request).Read
	if hardlink.DataBase64 != first.DataBase64 {
		t.Fatal("hardlink read changed requested bytes")
	}
	execute(t, f, List, f.request)
	execute(t, f, Inspect, f.request)
	execute(t, f, Git, f.request)
	execute(t, f, Scopes, f.request)
	if after := snapshot(t, f.root); !reflect.DeepEqual(before, after) {
		t.Fatal("browsing mutated Native root state")
	}
	write(t, filepath.Join(f.path, "hardlink"), []byte("replacement"))
	_, err = Execute(t.Context(), f.root, Read, request)
	requireCode(t, err, Changed)
}

func TestDirectoryPagesAreLosslessBoundedAndRestartAfterChanges(t *testing.T) {
	f := newFixture(t)
	names := []string{".hidden", "colon:name", "back\\slash", "control\x01", "white space", "new\nline", "日本語", string([]byte{'b', 0xff})}
	for index := range 207 {
		names = append(names, fmt.Sprintf("file%03d", index))
	}
	for _, name := range names {
		write(t, filepath.Join(f.path, name), []byte(name))
	}
	slices.Sort(names)
	request := f.request
	var observed []string
	var firstCursor string
	for {
		page := execute(t, f, List, request).List
		if len(page.Entries) > 100 || !page.EOF && len(page.Entries) != 100 {
			t.Fatalf("unbounded or incomplete page: %+v", page)
		}
		for _, entry := range page.Entries {
			name, err := base64.StdEncoding.DecodeString(entry.NameBase64)
			must(t, err)
			observed = append(observed, string(name))
			verifyRawNameRead(t, f, entry)
		}
		if firstCursor == "" {
			firstCursor = page.NextCursor
		}
		if page.EOF {
			if page.NextCursor != "" {
				t.Fatal("EOF page retained cursor")
			}
			break
		}
		request.Cursor = page.NextCursor
	}
	if !slices.Equal(names, observed) {
		t.Fatalf("pages changed raw name order or omitted entries: got %q want %q", observed, names)
	}
	write(t, filepath.Join(f.path, "new entry"), nil)
	request.Cursor = firstCursor
	_, err := Execute(t.Context(), f.root, List, request)
	requireCode(t, err, Changed)
}

func verifyRawNameRead(t *testing.T, f fixture, entry Entry) {
	t.Helper()
	if entry.Name != nil && !strings.ContainsAny(*entry.Name, "\\\n:\x01") {
		return
	}
	request := f.request
	request.RelativePathBase64 = entry.RelativePathBase64
	result := execute(t, f, Read, request).Read
	if result.DataBase64 != entry.NameBase64 {
		t.Fatal("raw name selector selected different file bytes")
	}
}

func TestSelectorsAndCursorsCannotRetargetAnotherScope(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.path, "a"), nil)
	write(t, filepath.Join(f.path, "b"), nil)
	request := f.request
	request.Limit = 1
	page := execute(t, f, List, request).List
	other := newFixture(t)
	otherRequest := other.request
	otherRequest.Cursor = page.NextCursor
	_, err := Execute(t.Context(), other.root, List, otherRequest)
	// Directory version is checked after selector binding; another selection
	// must be refused even if both directories happen to have identical bytes.
	requireCode(t, err, InvalidRequest)
	for _, path := range []string{"../outside", "/tmp", string([]byte{'x', 0}), string([]byte{0xff})} {
		request = f.request
		request.RelativePath = path
		_, err = Execute(t.Context(), f.root, Read, request)
		if err == nil || strings.Contains(err.Error(), path) {
			t.Fatalf("unsafe selector admitted or leaked into error: %q %v", path, err)
		}
	}
	request = f.request
	request.RelativePath, request.RelativePathBase64 = "a", "Yg=="
	_, err = Execute(t.Context(), f.root, Read, request)
	requireCode(t, err, InvalidRequest)
	request = f.request
	request.ExpectedRoot = strings.Repeat("0", 64)
	_, err = Execute(t.Context(), f.root, List, request)
	requireCode(t, err, Changed)
}

func TestSymlinksStayInsideTheSelectedRoot(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.path, "allowed"), []byte("inside"))
	write(t, filepath.Join(filepath.Dir(f.path), "outside"), []byte("forbidden"))
	must(t, os.Mkdir(filepath.Join(f.path, "sub"), 0o700))
	for name, target := range map[string]string{
		"good": "../allowed", "escape": "../../outside", "absolute": filepath.Join(f.path, "allowed"), "loop": "loop",
	} {
		must(t, os.Symlink(target, filepath.Join(f.path, "sub", name)))
	}
	request := f.request
	request.RelativePath = "sub/good"
	if result := execute(t, f, Read, request).Read; result.Text == nil || *result.Text != "inside" {
		t.Fatal("contained relative symlink did not read target")
	}
	for _, name := range []string{"escape", "absolute"} {
		request.RelativePath = "sub/" + name
		_, err := Execute(t.Context(), f.root, Read, request)
		requireCode(t, err, OutsideScope)
	}
	request.RelativePath = "sub/loop"
	_, err := Execute(t.Context(), f.root, Read, request)
	if err == nil {
		t.Fatal("looping symlink admitted")
	}
	// A user-controlled name resembling the private os.Root error must not
	// change another native error into an outside-scope diagnosis.
	name := "path escapes from parent"
	must(t, os.Symlink(name, filepath.Join(f.path, name)))
	request.RelativePath = name
	_, err = Execute(t.Context(), f.root, Read, request)
	requireCode(t, err, Changed)
}

func TestSymlinkParentsAndMultipleRelativeTargetsKeepNativeMeaning(t *testing.T) {
	f := newFixture(t)
	must(t, os.MkdirAll(filepath.Join(f.path, "actual", "child"), 0o700))
	write(t, filepath.Join(f.path, "actual", "target"), []byte("correct parent"))
	write(t, filepath.Join(f.path, "target"), []byte("wrong lexical parent"))
	must(t, os.Symlink("actual/child", filepath.Join(f.path, "alias")))
	must(t, os.Symlink("../indirect", filepath.Join(f.path, "actual", "child", "link")))
	must(t, os.Symlink("target", filepath.Join(f.path, "actual", "indirect")))
	request := f.request
	request.RelativePath = "alias/link"
	result := execute(t, f, Read, request).Read
	if result.Text == nil || *result.Text != "correct parent" {
		t.Fatalf("relative symlinks lost native parent semantics: %+v", result)
	}
	request.RelativePath = "alias/../target"
	result = execute(t, f, Read, request).Read
	if result.Text == nil || *result.Text != "correct parent" {
		t.Fatalf("explicit selector was lexically retargeted: %+v", result)
	}
	request.RelativePath = "alias/.."
	page := execute(t, f, List, request).List
	for _, entry := range page.Entries {
		if entry.Name != nil && *entry.Name == "target" {
			request.RelativePath = ""
			request.RelativePathBase64 = entry.RelativePathBase64
			result = execute(t, f, Read, request).Read
			if result.Text == nil || *result.Text != "correct parent" {
				t.Fatal("listed selector was lexically retargeted")
			}
		}
	}
}

func TestSpecialEntriesListButNeverReadAsContent(t *testing.T) {
	f := newFixture(t)
	must(t, syscall.Mkfifo(filepath.Join(f.path, "pipe"), 0o600))
	// Darwin's native temporary paths can exceed sockaddr_un's pathname
	// capacity. A short owned alias binds the socket in the same fixture tree.
	short, err := os.MkdirTemp("/tmp", "workspacefiles-socket-")
	must(t, err)
	t.Cleanup(func() { must(t, os.RemoveAll(short)) })
	alias := filepath.Join(short, "workspace")
	must(t, os.Symlink(f.path, alias))
	listener, err := net.Listen("unix", filepath.Join(alias, "socket"))
	must(t, err)
	t.Cleanup(func() { must(t, listener.Close()) })
	entries := execute(t, f, List, f.request).List.Entries
	if len(entries) != 2 || entries[0].Kind != FIFO || entries[1].Kind != Socket {
		t.Fatalf("special entries were hidden or misclassified: %+v", entries)
	}
	for _, name := range []string{"pipe", "socket"} {
		request := f.request
		request.RelativePath = name
		_, err = Execute(t.Context(), f.root, Read, request)
		requireCode(t, err, NotRegularFile)
	}
	identity, err := taskdir.ObservePathProjectContext(t.Context(), "/dev")
	must(t, err)
	token, err := taskdir.ProjectToken(identity)
	must(t, err)
	request := f.request
	request.Scope = Scope{Kind: Workspace, ExpectedWorkspace: token}
	request.RelativePath = "null"
	_, err = Execute(t.Context(), f.root, Read, request)
	requireCode(t, err, NotRegularFile)
}

func TestHeldScopeSurvivesPathReplacementAndDoesNotFollowNewTree(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.path, "file"), []byte("acquired"))
	selected, err := openScope(t.Context(), f.request.Scope)
	must(t, err)
	t.Cleanup(func() { must(t, selected.Close()) })
	must(t, os.Rename(f.path, f.path+"-old"))
	must(t, os.Mkdir(f.path, 0o700))
	write(t, filepath.Join(f.path, "file"), []byte("replacement"))
	result, err := selected.read(t.Context(), "file", Request{})
	must(t, err)
	if result.Text == nil || *result.Text != "acquired" {
		t.Fatal("held rooted selection switched to replacement tree")
	}
	request := f.request
	request.RelativePath = "file"
	if current := execute(t, f, Read, request).Read; current.Text == nil || *current.Text != "replacement" {
		t.Fatal("fresh path approval did not observe replacement")
	}
}

func TestConcurrentSymlinkReplacementCannotReturnOutsideBytes(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.path, "allowed"), []byte("inside"))
	write(t, filepath.Join(filepath.Dir(f.path), "outside"), []byte("forbidden"))
	link := filepath.Join(f.path, "link")
	must(t, os.Symlink("allowed", link))
	ctx, cancel := context.WithCancel(t.Context())
	var owner sync.WaitGroup
	owner.Add(1)
	fault := make(chan error, 1)
	go func() {
		defer owner.Done()
		for ctx.Err() == nil {
			for _, target := range []string{"../outside", "allowed"} {
				stage := link + ".stage"
				if err := os.Symlink(target, stage); err != nil {
					fault <- err
					return
				}
				if err := os.Rename(stage, link); err != nil {
					fault <- err
					return
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); owner.Wait() })
	request := f.request
	request.RelativePath = "link"
	for range 100 {
		result, err := Execute(t.Context(), f.root, Read, request)
		if err == nil && (result.Read.Text == nil || *result.Read.Text != "inside") {
			t.Fatal("replacement race escaped root")
		}
	}
	cancel()
	owner.Wait()
	select {
	case err := <-fault:
		t.Fatal(err)
	default:
	}
}

func TestPermissionAndAbsentRootAreExplicitWithoutInitialization(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(t.TempDir(), "missing-native")
	_, err := Execute(t.Context(), missing, List, f.request)
	requireCode(t, err, NotFound)
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read initialized missing Native root")
	}
	if os.Geteuid() == 0 {
		t.Skip("OS permission denial requires an ordinary user")
	}
	path := filepath.Join(f.path, "denied")
	write(t, path, []byte("unreadable"))
	must(t, os.Chmod(path, 0))
	t.Cleanup(func() { must(t, os.Chmod(path, 0o600)) })
	request := f.request
	request.RelativePath = "denied"
	_, err = Execute(t.Context(), f.root, Read, request)
	requireCode(t, err, PermissionDenied)
}

func TestLinuxAutomaticTempIgnoresTMPDIRAndNeedsNoNativeState(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux scope contract")
	}
	t.Setenv("TMPDIR", t.TempDir())
	result, err := Execute(t.Context(), "", Scopes, Request{})
	must(t, err)
	canonical, err := filepath.EvalSymlinks("/tmp")
	must(t, err)
	if result.Scopes == nil || len(result.Scopes.Scopes) != 1 || result.Scopes.Scopes[0].Directory != canonical || result.Scopes.Scopes[0].Kind != SharedTemp {
		t.Fatalf("temp scope selected environment override: %+v", result)
	}
	encoded, err := json.Marshal(result)
	must(t, err)
	if strings.Contains(string(encoded), "expected_root") || result.List != nil || result.Read != nil || result.Inspect != nil {
		t.Fatal("scope result violated typed observation envelope")
	}
}

func TestOptionsAndCancellationFailWithoutPartialData(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		operation Operation
		change    func(*Request)
	}{
		{Read, func(r *Request) { r.Offset = -1 }},
		{Read, func(r *Request) { r.Limit = MaxReadBytes + 1 }},
		{List, func(r *Request) { r.Limit = MaxEntries + 1 }},
		{Read, func(r *Request) { r.Cursor = "unexpected" }},
		{Inspect, func(r *Request) { r.Offset = 1 }},
		{Scopes, func(r *Request) { r.RelativePath = "ignored" }},
		{Read, func(r *Request) { r.Scope.Directory = filepath.Dir(f.path) }},
	}
	for index, test := range cases {
		request := f.request
		test.change(&request)
		result, err := Execute(t.Context(), f.root, test.operation, request)
		if err == nil || !reflect.DeepEqual(result, Result{}) {
			t.Fatalf("invalid option %d returned data: %+v %v", index, result, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := Execute(ctx, f.root, List, f.request)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("canceled observation returned partial success: %+v %v", result, err)
	}
}

func TestLargeEscapedDirectoryPagesRemainWithinTheFrameBudget(t *testing.T) {
	f := newFixture(t)
	path := f.path
	var segments []string
	for range 16 {
		segment := strings.Repeat("\x01", 200)
		segments = append(segments, segment)
		path = filepath.Join(path, segment)
		must(t, os.Mkdir(path, 0o700))
	}
	for index := range 100 {
		write(t, filepath.Join(path, fmt.Sprintf("entry%03d", index)), nil)
	}
	request := f.request
	request.RelativePath = filepath.Join(segments...)
	result, err := Execute(t.Context(), f.root, List, request)
	requireCode(t, err, ResourceLimit)
	if !reflect.DeepEqual(result, Result{}) {
		t.Fatal("oversized page escaped the response budget")
	}
	request.Limit = 10
	page := execute(t, f, List, request).List
	if len(page.Entries) != 10 || page.EOF || page.NextCursor == "" {
		t.Fatal("smaller explicit page could not access the same unfiltered names")
	}
}

func TestWholeSharedTempIncludesRequestedPrivateLookingFiles(t *testing.T) {
	f := newFixture(t)
	path, err := os.MkdirTemp("/tmp", "workspacefiles-whole-temp-")
	must(t, err)
	t.Cleanup(func() { must(t, os.RemoveAll(path)) })
	name := ".delegate-private-looking-file"
	write(t, filepath.Join(path, name), []byte("requested sentinel bytes"))
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	must(t, err)
	canonicalPath, err := filepath.EvalSymlinks(path)
	must(t, err)
	relative, err := filepath.Rel(tempRoot, filepath.Join(canonicalPath, name))
	must(t, err)
	request := f.request
	request.Scope = Scope{Kind: SharedTemp}
	request.RelativePath = relative
	result := execute(t, f, Read, request).Read
	if result.Text == nil || *result.Text != "requested sentinel bytes" {
		t.Fatal("whole-temp access filtered the requested file")
	}
}

func TestTempAliasesAreCanonicalizedAndDeduplicated(t *testing.T) {
	path := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(path, alias))
	scopes, err := canonicalTempScopes([]ObservedScope{
		{Kind: UserTemp, Directory: path}, {Kind: SharedTemp, Directory: alias},
	})
	must(t, err)
	if len(scopes) != 1 || scopes[0].Kind != UserTemp || scopes[0].ID == "" {
		t.Fatalf("canonical alias deduplication failed: %+v", scopes)
	}
}
