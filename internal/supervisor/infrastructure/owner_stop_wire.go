package infrastructure

import (
	"bufio"
	"encoding/json"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Stop compatibility is independent of preparation and record schemas. These
// bounded subjects name an existing owner; they grant no executable selection.
const ownerStopProtocolVersion = 1

type ownerStopRequest struct {
	StopProtocol      int              `json:"stop_protocol"`
	RootID            string           `json:"root_id"`
	Owner             StartupReference `json:"owner"`
	PublicationSHA256 string           `json:"publication_sha256"`
	Client            ProcessIdentity  `json:"client"`
	Deadline          string           `json:"deadline"`
}

type ownerStopCode string

const (
	ownerStopAccepted ownerStopCode = "accepted"
	ownerStopBusy     ownerStopCode = "busy"
	ownerStopFault    ownerStopCode = "fault"
)

type ownerStopReply struct {
	StopProtocol      int              `json:"stop_protocol"`
	RootID            string           `json:"root_id"`
	Owner             StartupReference `json:"owner"`
	PublicationSHA256 string           `json:"publication_sha256"`
	Code              ownerStopCode    `json:"code"`
}

func validateStopSubject(protocol int, rootID string, owner StartupReference, publication string) error {
	if protocol != ownerStopProtocolVersion {
		return ErrOwnerIncompatible
	}
	return errors.Join(task.ValidateRootID(rootID), owner.Validate(), task.ValidateSHA256(publication))
}

func (r ownerStopRequest) validate() error {
	if err := validateStopSubject(r.StopProtocol, r.RootID, r.Owner, r.PublicationSHA256); err != nil {
		return err
	}
	if _, err := r.Client.Reference(); err != nil {
		return err
	}
	deadline, err := canonicalStartupTime(r.Deadline)
	if err != nil {
		return err
	}
	if deadline.IsZero() {
		return ErrIdentity
	}
	return nil
}

func (r ownerStopReply) validate() error {
	if err := validateStopSubject(r.StopProtocol, r.RootID, r.Owner, r.PublicationSHA256); err != nil {
		return err
	}
	switch r.Code {
	case ownerStopAccepted, ownerStopBusy, ownerStopFault:
		return nil
	default:
		return ErrIdentity
	}
}

// Authentication consumes the existing local status subject. This conversion
// is server-local; no preparation data or protocol is accepted from stop wire.
func (r ownerStopRequest) authenticationRequest() ownerRequest {
	return ownerRequest{Protocol: ownerProtocolVersion, RootID: r.RootID, Owner: r.Owner, PublicationSHA256: r.PublicationSHA256, Client: r.Client, Deadline: r.Deadline, Action: ownerStatus}
}

func readOwnerControlFrame(input *bufio.Reader) (ownerRequest, ownerStopRequest, error) {
	data, err := readOwnerFrameBytes(input)
	if err != nil {
		return ownerRequest{}, ownerStopRequest{}, err
	}
	var selector struct {
		StopProtocol int `json:"stop_protocol"`
	}
	if err = json.Unmarshal(data, &selector); err != nil {
		return ownerRequest{}, ownerStopRequest{}, invalidOwnerFrame(err)
	}
	if selector.StopProtocol != 0 {
		stop, decodeErr := decodeOwnerFrame[ownerStopRequest](data)
		return ownerRequest{}, stop, decodeErr
	}
	request, err := decodeOwnerFrame[ownerRequest](data)
	return request, ownerStopRequest{}, err
}
