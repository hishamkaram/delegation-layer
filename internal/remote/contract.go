// Package remote defines transport-neutral remote operations and origin references.
// The destination lifecycle remains the only task execution authority.
package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	ProtocolVersion            = 1
	MaxControlBytes            = 1024 * 1024
	MaxDataBytes               = 64 * 1024
	DefaultOperationTimeout    = 30 * time.Second
	DefaultTransferTimeout     = 15 * time.Minute
	DefaultTransferIdleTimeout = 30 * time.Second
)

var (
	ErrAdmissionAlreadySent = errors.New("admission already sent; reconcile existing request")
	ErrAdmissionRejected    = errors.New("remote admission was rejected before task creation")
	ErrProtocol             = errors.New("invalid remote protocol")
	ErrIdentity             = errors.New("remote identity mismatch")
	ErrConflict             = errors.New("remote request conflict")
	ErrAcceptanceUnknown    = errors.New("remote acceptance is unknown; reconcile the same reference")
)

type Operation string

const (
	Hello       Operation = "hello"
	Prepare     Operation = "prepare"
	Admit       Operation = "admit"
	Reconcile   Operation = "reconcile"
	Status      Operation = "status"
	Collect     Operation = "collect"
	Cancel      Operation = "cancel"
	Events      Operation = "events"
	Reply       Operation = "reply"
	Steer       Operation = "steer"
	StageBegin  Operation = "stage-begin"
	StageChunk  Operation = "stage-chunk"
	StageCommit Operation = "stage-commit"
	Download    Operation = "download"
)

func (o Operation) Validate() error {
	switch o {
	case Hello, Prepare, Admit, Reconcile, Status, Collect, Cancel, Events, Reply, Steer, StageBegin, StageChunk, StageCommit, Download:
		return nil
	}
	return fmt.Errorf("%w: unsupported operation %q", ErrProtocol, o)
}

// Identity pins routing and trust; native directory identity is an observation.
type Identity struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	HostKey    string `json:"host_key"`
	Root       string `json:"root"`
	RootID     string `json:"root_id"`
	RootDevice uint64 `json:"root_device,omitempty"`
	RootInode  uint64 `json:"root_inode,omitempty"`
}

func (i Identity) Validate() error {
	if i.Host == "" || i.Port < 1 || i.Port > 65535 || i.User == "" || i.HostKey == "" || !filepath.IsAbs(i.Root) {
		return fmt.Errorf("%w: incomplete destination", ErrIdentity)
	}
	return task.ValidateRootID(i.RootID)
}

func (i Identity) Match(observed Identity) error {
	if i.Host != observed.Host || i.Port != observed.Port || i.User != observed.User || i.HostKey != observed.HostKey || i.Root != observed.Root || i.RootID != observed.RootID {
		return ErrIdentity
	}
	return nil
}

func (i Identity) PinChanged(observed Identity) bool {
	return i.RootDevice != observed.RootDevice || i.RootInode != observed.RootInode
}

type Descriptor struct {
	Executable bool   `json:"executable,omitempty"`
	Name       string `json:"name"`
	Length     int64  `json:"length"`
	SHA256     string `json:"sha256"`
}

// Intent is immutable after persistence. Paths describe remote paths or labels,
// never an implicit translation of an origin absolute path.
type Intent struct {
	InputSelectionSHA256 string          `json:"input_selection_sha256,omitempty"`
	SSHConfig            string          `json:"ssh_config,omitempty"`
	Version              int             `json:"version"`
	OriginRootID         string          `json:"origin_root_id"`
	TaskID               string          `json:"task_id"`
	Device               string          `json:"device"`
	Executable           string          `json:"executable"`
	Destination          Identity        `json:"destination"`
	Provider             string          `json:"provider"`
	Config               task.TaskConfig `json:"config"`
	RemoteCwd            string          `json:"remote_cwd"`
	Brief                Descriptor      `json:"brief"`
	InputSHA256          string          `json:"input_sha256,omitempty"`
	OutputPaths          []string        `json:"output_paths,omitempty"`
}

func (i Intent) Validate() error {
	if i.SSHConfig != "" && !filepath.IsAbs(i.SSHConfig) {
		return fmt.Errorf("%w: SSH config path must be absolute", ErrProtocol)
	}
	if i.Version != ProtocolVersion || i.Provider == "" || i.Device == "" || !filepath.IsAbs(i.RemoteCwd) {
		return fmt.Errorf("%w: invalid intent", ErrProtocol)
	}
	if err := task.ValidateRootID(i.OriginRootID); err != nil {
		return err
	}
	if err := task.ValidateTaskID(i.TaskID); err != nil {
		return err
	}
	if err := i.Destination.Validate(); err != nil {
		return err
	}
	if err := i.validateInputDescriptors(); err != nil {
		return err
	}
	return validateOutputPaths(i.OutputPaths)
}

func (i Intent) validateInputDescriptors() error {
	if i.Brief.Length <= 0 || i.Brief.Length > task.MaxBriefSize || !validSHA(i.Brief.SHA256) {
		return fmt.Errorf("%w: invalid brief descriptor", ErrProtocol)
	}
	if i.InputSelectionSHA256 != "" && !validSHA(i.InputSelectionSHA256) {
		return fmt.Errorf("%w: invalid selection digest", ErrProtocol)
	}
	if i.InputSHA256 != "" && !validSHA(i.InputSHA256) {
		return fmt.Errorf("%w: invalid input digest", ErrProtocol)
	}
	return nil
}

func validateOutputPaths(paths []string) error {
	if err := remotefiles.ValidateSelectors(paths); err != nil {
		return err
	}
	if !slices.IsSorted(paths) {
		return fmt.Errorf("%w: output paths must be ordered", ErrProtocol)
	}
	return nil
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type Control struct {
	ID      string                   `json:"id"`
	Message *task.InteractionMessage `json:"message,omitempty"`
}

type TransferRequest struct {
	Selector string `json:"selector"`
	Offset   int64  `json:"offset"`
	Length   int64  `json:"length"`
	SHA256   string `json:"sha256,omitempty"`
}

type Request struct {
	BeforeWrite  func() error          `json:"-"`
	IntentSHA256 string                `json:"intent_sha256,omitempty"`
	Provider     string                `json:"provider,omitempty"`
	Cwd          string                `json:"cwd,omitempty"`
	Manifest     *remotefiles.Manifest `json:"-"`
	Data         []byte                `json:"-"`
	Version      int                   `json:"version"`
	Operation    Operation             `json:"operation"`
	Expected     *Identity             `json:"expected,omitempty"`
	Root         string                `json:"root,omitempty"`
	TaskID       string                `json:"task_id,omitempty"`
	Intent       *Intent               `json:"intent,omitempty"`
	Control      *Control              `json:"control,omitempty"`
	After        uint64                `json:"after,omitempty"`
	WatchNanos   int64                 `json:"watch_nanos,omitempty"`
	Transfer     *TransferRequest      `json:"transfer,omitempty"`
}

func (r Request) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("%w: version %d", ErrProtocol, r.Version)
	}
	if err := r.Operation.Validate(); err != nil {
		return err
	}
	if r.TaskID != "" {
		if err := task.ValidateTaskID(r.TaskID); err != nil {
			return err
		}
	}
	if r.Expected != nil {
		if err := r.Expected.Validate(); err != nil {
			return err
		}
	}
	if r.Intent != nil {
		if err := r.Intent.Validate(); err != nil {
			return err
		}
	}

	if r.WatchNanos < 0 || r.WatchNanos > int64(DefaultOperationTimeout) {
		return fmt.Errorf("%w: observation wait exceeds bound", ErrProtocol)
	}
	if r.Transfer != nil && (r.Transfer.Offset < 0 || r.Transfer.Length < 0) {
		return fmt.Errorf("%w: invalid transfer range", ErrProtocol)
	}
	return r.validateBindings()
}

func (r Request) validateBindings() error {
	if r.Intent != nil && r.TaskID != "" && r.Intent.TaskID != r.TaskID {
		return ErrConflict
	}
	if r.Expected != nil && r.Intent != nil {
		if err := r.Expected.Match(r.Intent.Destination); err != nil {
			return err
		}
	}
	if r.Control != nil {
		if err := task.ValidateTaskID(r.Control.ID); err != nil {
			return err
		}
	}

	return nil
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type TransferReceipt struct {
	Capture      *remotefiles.CapturedFile `json:"capture,omitempty"`
	Provenance   *remotefiles.Provenance   `json:"provenance,omitempty"`
	Availability string                    `json:"availability,omitempty"`
	Length       int64                     `json:"length"`
	Descriptor   Descriptor                `json:"descriptor"`
	Offset       int64                     `json:"offset"`
	Complete     bool                      `json:"complete"`
}

// Response.Envelope is the bounded original Delegate response, validated at the
// protocol boundary. It is carried rather than reduced into another task model.
type Response struct {
	TaskRecord     string           `json:"task_record,omitempty"`
	Diagnostics    []string         `json:"diagnostics,omitempty"`
	Data           []byte           `json:"-"`
	Version        int              `json:"version"`
	Identity       Identity         `json:"identity"`
	Capabilities   []string         `json:"capabilities,omitempty"`
	Envelope       json.RawMessage  `json:"response,omitempty"`
	Failure        *Failure         `json:"failure,omitempty"`
	Transfer       *TransferReceipt `json:"transfer,omitempty"`
	Existing       bool             `json:"existing,omitempty"`
	RequestSHA256  string           `json:"request_sha256,omitempty"`
	RootPinChanged bool             `json:"root_pin_changed,omitempty"`
}

// Validate checks an origin response after Exchange attaches the authenticated
// SSH endpoint identity. Root-only wire responses are checked by the framer.
func (r Response) Validate() error {
	if err := r.validateWire(); err != nil {
		return err
	}
	if r.Identity.RootID != "" {
		return r.Identity.Validate()
	}
	return nil
}

func (r Response) validateWire() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("%w: unsupported response version", ErrProtocol)
	}
	if r.Identity.RootID != "" {
		if err := task.ValidateRootID(r.Identity.RootID); err != nil {
			return err
		}
		if !filepath.IsAbs(r.Identity.Root) {
			return fmt.Errorf("%w: invalid root path", ErrProtocol)
		}
	} else if r.Failure == nil && len(r.Capabilities) == 0 {
		return fmt.Errorf("%w: response has no root identity", ErrProtocol)
	}
	envelope := bytes.TrimSpace(r.Envelope)
	if len(envelope) > MaxControlBytes || (len(envelope) > 0 && (!json.Valid(envelope) || envelope[0] != '{')) {
		return fmt.Errorf("%w: invalid Delegate envelope", ErrProtocol)
	}
	return nil
}

func Handle(id string) string { return "remote:" + id }
func ParseHandle(handle string) (string, error) {
	id, ok := strings.CutPrefix(handle, "remote:")
	if !ok {
		return "", fmt.Errorf("%w: expected remote:ID", ErrProtocol)
	}
	return id, task.ValidateTaskID(id)
}
