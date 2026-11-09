package remote

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
)

// Framer belongs to one channel; callers must serialize writes and reads.
type Framer struct {
	Reader io.Reader
	Writer io.Writer
}

func (f Framer) WriteControl(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return f.writeFrame(data, MaxControlBytes)
}

func (f Framer) ReadControl(value any) error {
	data, err := f.readFrame(MaxControlBytes)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if err = decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON", ErrProtocol)
	}
	return nil
}

func (f Framer) WriteData(data []byte) error { return f.writeFrame(data, MaxDataBytes) }
func (f Framer) ReadData() ([]byte, error)   { return f.readFrame(MaxDataBytes) }
func (f Framer) writeFrame(data []byte, limit int) error {
	if len(data) == 0 || len(data) > limit {
		return fmt.Errorf("%w: frame size exceeds %d", ErrProtocol, limit)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if err := writeAll(f.Writer, header[:]); err != nil {
		return err
	}
	return writeAll(f.Writer, data)
}

func (f Framer) readFrame(limit int) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(f.Reader, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > uint32(limit) {
		return nil, fmt.Errorf("%w: invalid frame size", ErrProtocol)
	}
	data := make([]byte, int(length))
	_, err := io.ReadFull(f.Reader, data)
	return data, err
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// WriteRequest/ReadRequest keep brief/data bytes outside control JSON. Each data
// frame remains bounded and the declared total is checked before allocation.
func (f Framer) WriteRequest(request Request) error {
	var prepErr error
	request, prepErr = prepareManifest(request)
	if prepErr != nil {
		return prepErr
	}
	if err := request.Validate(); err != nil {
		return err
	}
	length, err := requestDataLength(request)
	if err != nil {
		return err
	}
	if int64(len(request.Data)) != length {
		return fmt.Errorf("%w: request data length", ErrProtocol)
	}
	control, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(control) == 0 || len(control) > MaxControlBytes {
		return fmt.Errorf("%w: request control exceeds bound", ErrProtocol)
	}
	if request.BeforeWrite != nil {
		if err = request.BeforeWrite(); err != nil {
			return err
		}
	}
	if err = f.writeFrame(control, MaxControlBytes); err != nil {
		return err
	}
	return f.writeData(request.Data)
}

func (f Framer) ReadRequest() (Request, error) {
	var request Request
	if err := f.ReadControl(&request); err != nil {
		return request, err
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	length, err := requestDataLength(request)
	if err != nil {
		return request, err
	}
	request.Data, err = f.readData(length)
	if err == nil && (request.Operation == Admit || request.Operation == Reconcile) && DigestBytes(request.Data) != request.Intent.Brief.SHA256 {
		return request, fmt.Errorf("%w: brief digest mismatch", ErrProtocol)
	}
	if err == nil && request.Operation == StageBegin && request.Transfer != nil && request.Transfer.Selector == "manifest" {
		err = decodeManifest(&request)
	}
	return request, err
}

func requestDataLength(request Request) (int64, error) {
	switch request.Operation {
	case Admit, Reconcile:
		if request.Intent == nil {
			return 0, fmt.Errorf("%w: admission intent required", ErrProtocol)
		}
		return request.Intent.Brief.Length, nil
	case StageChunk:
		if request.Transfer == nil || request.Transfer.Length > MaxDataBytes {
			return 0, fmt.Errorf("%w: invalid chunk length", ErrProtocol)
		}
		return request.Transfer.Length, nil
	case StageBegin:
		if request.Transfer != nil && request.Transfer.Selector == "manifest" {
			if request.Transfer.Length <= 0 || request.Transfer.Length > MaxManifestBytes {
				return 0, fmt.Errorf("%w: manifest exceeds limit", ErrProtocol)
			}
			return request.Transfer.Length, nil
		}
		return 0, nil
	case Hello, Prepare, Status, Collect, Cancel, Events, Reply, Steer, StageCommit, Download, Usage, Retire:
		return 0, nil
	}
	return 0, ErrProtocol
}

func (f Framer) WriteResponse(response Response) error {
	if err := response.validateWire(); err != nil {
		return err
	}
	var length int64
	if response.Transfer != nil {
		length = response.Transfer.Length
	}
	if length < 0 || length > MaxDataBytes || int64(len(response.Data)) != length {
		return fmt.Errorf("%w: response data length", ErrProtocol)
	}
	if err := f.WriteControl(response); err != nil {
		return err
	}
	return f.writeData(response.Data)
}

func (f Framer) ReadResponse() (Response, error) {
	var response Response
	if err := f.ReadControl(&response); err != nil {
		return response, err
	}
	if err := response.validateWire(); err != nil {
		return response, err
	}
	var length int64
	if response.Transfer != nil {
		length = response.Transfer.Length
	}
	if length < 0 || length > MaxDataBytes {
		return response, fmt.Errorf("%w: invalid response data length", ErrProtocol)
	}
	data, err := f.readData(length)
	response.Data = data
	return response, err
}

func (f Framer) writeData(data []byte) error {
	for len(data) > 0 {
		count := len(data)
		if count > MaxDataBytes {
			count = MaxDataBytes
		}
		if err := f.WriteData(data[:count]); err != nil {
			return err
		}
		data = data[count:]
	}
	return nil
}

func (f Framer) readData(length int64) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}
	result := make([]byte, 0, int(length))
	for int64(len(result)) < length {
		data, err := f.ReadData()
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > length-int64(len(result)) {
			return nil, fmt.Errorf("%w: data exceeds declared length", ErrProtocol)
		}
		result = append(result, data...)
	}
	return result, nil
}

const MaxManifestBytes = 4 * 1024 * 1024

func prepareManifest(request Request) (Request, error) {
	if request.Manifest == nil {
		return request, nil
	}
	if request.Operation != StageBegin {
		return request, fmt.Errorf("%w: manifest only valid for stage-begin", ErrProtocol)
	}
	digest, err := remotefiles.ManifestSHA256(*request.Manifest)
	if err != nil {
		return request, err
	}
	data, err := json.Marshal(request.Manifest)
	if err != nil {
		return request, err
	}
	if len(data) > MaxManifestBytes {
		return request, fmt.Errorf("%w: manifest exceeds limit", ErrProtocol)
	}
	descriptor := TransferRequest{Selector: "manifest", Length: int64(len(data)), SHA256: digest}
	if request.Transfer != nil && *request.Transfer != descriptor {
		return request, ErrConflict
	}
	request.Transfer = &descriptor
	request.Data = data
	return request, nil
}

func decodeManifest(request *Request) error {
	var manifest remotefiles.Manifest
	decoder := json.NewDecoder(bytes.NewReader(request.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("%w: invalid manifest: %w", ErrProtocol, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing manifest data", ErrProtocol)
	}
	digest, err := remotefiles.ManifestSHA256(manifest)
	if err != nil {
		return err
	}
	if digest != request.Transfer.SHA256 {
		return fmt.Errorf("%w: manifest digest mismatch", ErrProtocol)
	}
	request.Manifest = &manifest
	return nil
}
