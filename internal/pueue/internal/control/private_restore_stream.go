package control

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type privateRestoreReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r privateRestoreReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(buffer)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, errors.Join(err, contextErr)
	}
	return n, err
}

func parsePrivateRestoreStream(ctx context.Context, input io.Reader, compressed bool, version string) (snapshot QueueSnapshot, resultErr error) {
	source := &privateRestoreInputReader{reader: input}
	var closeErr error
	defer func() {
		operational := errors.Join(source.fault, ctx.Err())
		if operational == nil {
			resultErr = privateRestoreFault(ErrPrivateRestoreCorrupt, resultErr)
		}
		resultErr = errors.Join(resultErr, operational, closeErr)
		if resultErr != nil {
			snapshot = QueueSnapshot{}
		}
	}()
	reader := privateRestoreReader{ctx, source}
	if !compressed {
		return protocol.ParseQueueSnapshotStream(reader, version)
	}
	buffered := bufio.NewReader(reader)
	decoder, err := gzip.NewReader(buffered)
	if err != nil {
		return QueueSnapshot{}, err
	}
	defer func() { closeErr = decoder.Close() }()
	decoder.Multistream(false)
	snapshot, err = protocol.ParseQueueSnapshotStream(privateRestoreReader{ctx, decoder}, version)
	if err != nil {
		return QueueSnapshot{}, err
	}
	if err = ctx.Err(); err != nil {
		return QueueSnapshot{}, err
	}
	_, err = buffered.ReadByte()
	if !task.AllErrorCauses(err, privateRestoreEOF) {
		return QueueSnapshot{}, errors.Join(ErrUnknown, err)
	}
	return snapshot, ctx.Err()
}

func privateRestoreEOF(err error) bool { return errors.Is(err, io.EOF) }
