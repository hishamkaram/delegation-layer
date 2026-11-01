package contract

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Request is the immutable dispatch request a schedule replays for each
// occurrence. It records only what the caller supplied: either Auto with an
// empty Provider, or one structurally valid Provider. Omitted options stay
// empty; ordinary admission selects the provider and applies its defaults for
// each occurrence. Per-turn identities such as a prior session or workspace
// binding are absent by type.
type Request struct {
	Provider     string           `json:"provider,omitempty"`
	Auto         bool             `json:"auto,omitempty"`
	CanonicalCwd string           `json:"canonical_cwd"`
	Config       task.TaskConfig  `json:"config"`
	Team         *task.TeamTarget `json:"team,omitempty"`
}

// MarshalRequest validates a supplied request without probing the filesystem,
// credentials, or providers, and returns its canonical stored encoding, bounded
// to one control record.
func MarshalRequest(request Request) ([]byte, error) {
	return encodeRequest(request)
}

// DecodeRequest strictly decodes a stored request and accepts only valid
// requests in their exact canonical encoding.
func DecodeRequest(data []byte) (Request, error) {
	var request Request
	if err := task.DecodeStrict(data, &request); err != nil {
		return Request{}, fmt.Errorf("schedule request: %w", err)
	}
	canonical, err := encodeRequest(request)
	if err != nil {
		return Request{}, err
	}
	if !bytes.Equal(canonical, data) {
		return Request{}, errors.New("schedule request is not canonically encoded")
	}
	return request, nil
}

func encodeRequest(request Request) ([]byte, error) {
	if err := validateRequestFields(request); err != nil {
		return nil, err
	}
	encoded, err := task.MarshalCanonical(request)
	if err != nil {
		return nil, fmt.Errorf("schedule request: %w", err)
	}
	if len(encoded) > task.MaxControlRecordSize {
		return nil, fmt.Errorf("schedule request: %w", task.ErrControlRecordTooBig)
	}
	// encoding/json replaces invalid UTF-8 with U+FFFD, so a value can encode
	// as a different string than supplied. The stored bytes must decode to
	// exactly the request that was validated and digested.
	var stored Request
	if decodeErr := task.DecodeStrict(encoded, &stored); decodeErr != nil {
		return nil, fmt.Errorf("schedule request: %w", decodeErr)
	}
	if !sameRequest(stored, request) {
		return nil, errors.New("schedule request contains a string that canonical JSON cannot represent exactly, such as invalid UTF-8")
	}
	return encoded, nil
}

func sameRequest(left, right Request) bool {
	if !reflect.DeepEqual(left.Team, right.Team) {
		return false
	}
	left.Team, right.Team = nil, nil
	leftTarget, rightTarget := left.Config.ReviewTarget, right.Config.ReviewTarget
	left.Config.ReviewTarget, right.Config.ReviewTarget = nil, nil
	if left != right {
		return false
	}
	if leftTarget == nil || rightTarget == nil {
		return leftTarget == rightTarget
	}
	return *leftTarget == *rightTarget
}

func validateRequestFields(request Request) error {
	if err := validateRequestSelection(request); err != nil {
		return err
	}
	if !task.IsCanonicalCwd(request.CanonicalCwd) {
		return errors.New("schedule request cwd must be a clean absolute path")
	}
	if request.Config.Interactive {
		return errors.New("scheduled requests cannot be interactive")
	}
	if err := task.ValidateRequestedOptions(request.Config); err != nil {
		return fmt.Errorf("schedule request options: %w", err)
	}
	return nil
}

func validateRequestSelection(request Request) error {
	if request.Team == nil {
		return validateSelection(request.Provider, request.Auto)
	}
	if err := task.ValidateTeamTarget(*request.Team, true); err != nil {
		return err
	}
	if request.Provider != "" || request.Auto {
		return validateSelection(request.Provider, request.Auto)
	}
	return nil
}

func validateSelection(provider string, auto bool) error {
	if auto {
		if provider != "" {
			return errors.New("schedule request must select either auto or one provider, not both")
		}
		return nil
	}
	if provider == "" {
		return errors.New("schedule request must select auto or one provider")
	}
	if err := policy.ValidateProvider(provider); err != nil {
		return fmt.Errorf("schedule request provider: %w", err)
	}
	return nil
}

// clone returns a request whose review target no longer aliases the input.
func (request Request) clone() Request {
	if request.Team != nil {
		target := request.Team.Clone()
		request.Team = &target
	}
	if request.Config.ReviewTarget != nil {
		target := *request.Config.ReviewTarget
		request.Config.ReviewTarget = &target
	}
	return request
}
