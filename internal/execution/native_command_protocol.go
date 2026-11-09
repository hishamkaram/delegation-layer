package execution

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const nativeCommandMode = "--native-command-owner"

type nativeCommandRequest struct {
	NullOutput       bool     `json:"null_output,omitempty"`
	ID               string   `json:"id"`
	ExecutableBase64 string   `json:"executable_base64"`
	ArgumentsBase64  []string `json:"arguments_base64"`
	DirectoryBase64  string   `json:"directory_base64"`
	Deadline         string   `json:"deadline"`
}

type nativeCommandIdentity struct {
	nullOutput                bool
	id, executable, directory string
	arguments                 []string
	deadline                  time.Time
}

type nativeCommandResult struct {
	ExitCode int    `json:"exit_code"`
	Exited   bool   `json:"exited"`
	Fault    string `json:"fault,omitempty"`
	Canceled bool   `json:"canceled,omitempty"`
	Expired  bool   `json:"expired,omitempty"`
}

type nativeCommandExitError struct{ code int }

func (e nativeCommandExitError) Error() string { return "native command exited unsuccessfully" }
func (e nativeCommandExitError) ExitCode() int { return e.code }
func (e nativeCommandExitError) Exited() bool  { return true }

func validateNativeCommand(request nativeCommandRequest) error {
	_, err := decodeNativeCommand(request)
	return err
}

func decodeNativeCommand(request nativeCommandRequest) (nativeCommandIdentity, error) {
	var identity nativeCommandIdentity
	if err := task.ValidateTaskID(request.ID); err != nil {
		return identity, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, request.Deadline)
	if err != nil || deadline.IsZero() {
		return identity, errors.New("invalid native command deadline")
	}
	if len(request.ArgumentsBase64) == 0 || len(request.ArgumentsBase64) > 1024 {
		return identity, errors.New("invalid native command argument count")
	}
	executable, err := base64.StdEncoding.Strict().DecodeString(request.ExecutableBase64)
	if err != nil {
		return identity, fmt.Errorf("decode native executable: %w", err)
	}
	directory, err := base64.StdEncoding.Strict().DecodeString(request.DirectoryBase64)
	if err != nil {
		return identity, fmt.Errorf("decode native directory: %w", err)
	}
	identity = nativeCommandIdentity{nullOutput: request.NullOutput, id: request.ID, executable: string(executable), directory: string(directory), deadline: deadline, arguments: make([]string, len(request.ArgumentsBase64))}
	for index, encoded := range request.ArgumentsBase64 {
		argument, decodeErr := base64.StdEncoding.Strict().DecodeString(encoded)
		if decodeErr != nil {
			return nativeCommandIdentity{}, fmt.Errorf("decode native argument %d: %w", index, decodeErr)
		}
		identity.arguments[index] = string(argument)
	}
	return identity, validateNativeIdentity(identity)
}

func validateNativeIdentity(identity nativeCommandIdentity) error {
	if !filepath.IsAbs(identity.executable) || !filepath.IsAbs(identity.directory) {
		return errors.New("invalid native command path")
	}
	if strings.ContainsRune(identity.executable, 0) || strings.ContainsRune(identity.directory, 0) {
		return errors.New("invalid native command path")
	}
	for _, argument := range identity.arguments {
		if strings.ContainsRune(argument, 0) {
			return errors.New("invalid native command argument")
		}
	}
	return nil
}

// The framing follows the existing one-shot control owner. Only immutable
// command identity crosses it; environment and output use inherited streams.
func writeNativeFrame[T any](writer io.Writer, value T) error {
	data, err := task.MarshalCanonical(value)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > task.MaxControlRecordSize {
		return task.ErrControlRecordTooBig
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if err := writeNativeBytes(writer, header[:]); err != nil {
		return err
	}
	return writeNativeBytes(writer, data)
}

func writeNativeBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readNativeFrame[T any](reader io.Reader) (T, error) {
	var value T
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return value, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > task.MaxControlRecordSize {
		return value, task.ErrControlRecordTooBig
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return value, err
	}
	if err := task.DecodeStrict(data, &value); err != nil {
		return value, err
	}
	return value, nil
}

func nativeConnection(file *os.File) (*net.UnixConn, error) {
	connection, err := net.FileConn(file)
	closeErr := file.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok || closeErr != nil {
		return nil, errors.Join(errors.New("native command IPC requires a Unix connection"), closeErr, connection.Close())
	}
	return unixConnection, nil
}

func nativeSocketPair() (*net.UnixConn, *os.File, error) {
	// Darwin has no SOCK_CLOEXEC flag. Match Go's fork/exec boundary so no
	// concurrent child can inherit either endpoint during flag installation.
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		syscall.ForkLock.RUnlock()
		return nil, nil, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	syscall.ForkLock.RUnlock()
	parent := os.NewFile(uintptr(fds[0]), "native-command-parent")
	child := os.NewFile(uintptr(fds[1]), "native-command-child")
	connection, err := nativeConnection(parent)
	if err != nil {
		return nil, nil, errors.Join(err, child.Close())
	}
	return connection, child, nil
}
