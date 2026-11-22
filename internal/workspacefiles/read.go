package workspacefiles

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

// openRegular checks kind before opening, uses a nonblocking open for a raced
// FIFO, and checks the held descriptor before reading. Symlink containment is
// enforced by os.Root; hard links have no additional policy here.
func (s *selection) openRegular(path string) (_ *os.File, resultErr error) {
	before, err := s.root.Stat(path)
	if err != nil {
		return nil, classify("file_stat", err)
	}
	if !before.Mode().IsRegular() {
		return nil, failure(NotRegularFile, "file_kind", nil)
	}
	file, err := openContent(s.root, path)
	if err != nil {
		return nil, classify("file_open", err)
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.Join(failure(Changed, "file_open", err), classify("file_close", file.Close()))
	}
	return file, nil
}

func (s *selection) read(ctx context.Context, path string, request Request) (_ *ReadResult, resultErr error) {
	file, err := s.openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, classify("file_close", file.Close())) }()
	before, err := file.Stat()
	if err != nil {
		return nil, classify("file_stat", err)
	}
	version, err := metadataVersion(before)
	if err != nil {
		return nil, err
	}
	if err = requireVersion(request.ExpectedVersion, version); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, classify("file_read", err)
	}
	limit := request.Limit
	if limit == 0 {
		limit = MaxReadBytes
	}
	data := make([]byte, limit)
	n, err := file.ReadAt(data, request.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, classify("file_read", err)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, classify("file_stat", err)
	}
	afterVersion, err := metadataVersion(after)
	if err != nil {
		return nil, err
	}
	if version != afterVersion {
		return nil, failure(Changed, "file_read", nil)
	}
	data = data[:n]
	next := request.Offset + int64(n)
	return &ReadResult{
		DataBase64: base64.StdEncoding.EncodeToString(data), Text: textValue(string(data)),
		Offset: request.Offset, NextOffset: next, EOF: next >= after.Size(), Version: version,
	}, nil
}
