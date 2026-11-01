package execution

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestNativeCommandFramePreservesTypedIdentity(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "project with spaces")
	want := nativeTestRequest(filepath.Join(t.TempDir(), "git"), directory, []string{"git", "-C", directory, "rev-parse", "--show-toplevel"})
	var frame bytes.Buffer
	if err := writeNativeFrame(&frame, want); err != nil {
		t.Fatal(err)
	}
	got, err := readNativeFrame[nativeCommandRequest](&frame)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("transport changed command identity: %+v, %v", got, err)
	}
	if err := validateNativeCommand(got); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCommandFramePreservesNativeBytes(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "project-\xff")
	executable, arguments := filepath.Join(directory, "git-\xfe"), []string{"git", "", "literal-\xfd"}
	want := nativeTestRequest(executable, directory, arguments)
	var frame bytes.Buffer
	if err := writeNativeFrame(&frame, want); err != nil {
		t.Fatal(err)
	}
	got, err := readNativeFrame[nativeCommandRequest](&frame)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("transport changed native path or argument bytes: %v", err)
	}
	identity, err := decodeNativeCommand(got)
	if err != nil || identity.executable != executable || identity.directory != directory || !reflect.DeepEqual(identity.arguments, arguments) {
		t.Fatalf("decoding changed native identity: %+v, %v", identity, err)
	}
}

func nativeTestRequest(executable, directory string, arguments []string) nativeCommandRequest {
	encoded := make([]string, len(arguments))
	for index, argument := range arguments {
		encoded[index] = base64.StdEncoding.EncodeToString([]byte(argument))
	}
	return nativeCommandRequest{ID: "0123456789abcdef0123456789abcdef", ExecutableBase64: base64.StdEncoding.EncodeToString([]byte(executable)), ArgumentsBase64: encoded, DirectoryBase64: base64.StdEncoding.EncodeToString([]byte(directory)), Deadline: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339Nano)}
}

func TestNativeCommandRejectsInvalidEncodingAndDecodedIdentity(t *testing.T) {
	root := t.TempDir()
	for _, edit := range []func(*nativeCommandRequest){
		func(r *nativeCommandRequest) { r.ExecutableBase64 = "!" },
		func(r *nativeCommandRequest) { r.DirectoryBase64 = "!" },
		func(r *nativeCommandRequest) { r.ArgumentsBase64 = []string{"!"} },
		func(r *nativeCommandRequest) {
			r.ExecutableBase64 = base64.StdEncoding.EncodeToString([]byte("relative"))
		},
		func(r *nativeCommandRequest) {
			r.DirectoryBase64 = base64.StdEncoding.EncodeToString([]byte(filepath.Join(root, "nul\x00")))
		},
		func(r *nativeCommandRequest) {
			r.ArgumentsBase64 = []string{base64.StdEncoding.EncodeToString([]byte("nul\x00"))}
		},
		func(r *nativeCommandRequest) { r.ArgumentsBase64 = nil },
	} {
		request := nativeTestRequest(filepath.Join(root, "executable"), filepath.Join(root, "directory"), []string{"command"})
		edit(&request)
		if _, err := decodeNativeCommand(request); err == nil {
			t.Fatal("invalid encoded native command accepted")
		}
	}
}

func TestNativeCommandFrameRejectsInvalidBoundary(t *testing.T) {
	for _, data := range [][]byte{nil, {0, 0, 0, 0}, {0, 0, 0, 3, '{'}} {
		_, err := readNativeFrame[nativeCommandRequest](bytes.NewReader(data))
		if err == nil {
			t.Fatalf("truncated or empty frame accepted: %q", data)
		}
	}
	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], uint32(task.MaxControlRecordSize+1))
	if _, err := readNativeFrame[nativeCommandRequest](bytes.NewReader(oversized[:])); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized frame: %v", err)
	}
	var frame bytes.Buffer
	if err := writeNativeFrame(&frame, nativeCommandResult{ExitCode: 0, Exited: true}); err != nil {
		t.Fatal(err)
	}
	frame.Truncate(frame.Len() - 1)
	if _, err := readNativeFrame[nativeCommandResult](&frame); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated payload: %v", err)
	}
}

func TestNativeCommandNaturalExitIsSeparateFromCleanupAndCancellation(t *testing.T) {
	clean := nativeRecordedError(nativeCommandResult{ExitCode: 128, Exited: true})
	var exit interface{ ExitCode() int }
	if !errors.As(clean, &exit) || exit.ExitCode() != 128 {
		t.Fatalf("natural exit lost: %v", clean)
	}
	mixed := nativeRecordedError(nativeCommandResult{ExitCode: 128, Exited: true, Fault: "cleanup failed", Canceled: true})
	leaves := 0
	pure := task.AllErrorCauses(mixed, func(cause error) bool { leaves++; var code interface{ ExitCode() int }; return errors.As(cause, &code) })
	if pure || leaves == 0 || !errors.Is(mixed, context.Canceled) {
		t.Fatalf("cleanup/cancellation hidden behind exit: %v", mixed)
	}
	if err := nativeRecordedError(nativeCommandResult{}); err == nil {
		t.Fatal("empty completion accepted as success")
	}
}
