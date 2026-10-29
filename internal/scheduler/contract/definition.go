package contract

import (
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// MaxOpaqueTextBytes bounds the stored cron and timezone text. Their syntax
// belongs to the recurrence owner, which validates them before exposing create.
const MaxOpaqueTextBytes = 256

// definitionDigestVersion versions the canonical definition digest record.
const definitionDigestVersion = 1

// DefinitionHeader holds every immutable definition field except the brief, so
// metadata-only reads can verify the definition digest without loading it.
type DefinitionHeader struct {
	ScheduleID string
	CreatedAt  time.Time
	Cron       string
	Timezone   string
	Request    Request
}

// Definition is the immutable schedule definition. It never changes after
// creation, including after the schedule is deleted.
type Definition struct {
	DefinitionHeader
	Brief []byte
}

// Digests binds a definition. BriefSHA256 lets a later task and a summary
// refer to the brief without reading it; DefinitionSHA256 binds the immutable
// header to that brief digest. Mutable state and cursor are not bound.
type Digests struct {
	BriefSHA256      string
	DefinitionSHA256 string
}

type definitionDigestRecord struct {
	DigestVersion      int     `json:"digest_version"`
	ScheduleID         string  `json:"schedule_id"`
	CreatedAtUnixNanos int64   `json:"created_at_unix_nanos"`
	Cron               string  `json:"cron"`
	Timezone           string  `json:"timezone"`
	Request            Request `json:"request"`
	BriefSHA256        string  `json:"brief_sha256"`
}

// Clone returns a definition that shares no mutable memory with the input.
func (d Definition) Clone() Definition {
	d.Request = d.Request.clone()
	d.Brief = append([]byte(nil), d.Brief...)
	return d
}

// ValidateDefinition checks a definition before it is stored and after it is read.
func ValidateDefinition(d Definition) error {
	if err := validateHeader(d.DefinitionHeader); err != nil {
		return err
	}
	return validateBriefLength(int64(len(d.Brief)))
}

// DefinitionDigests validates d and computes its digests.
func DefinitionDigests(d Definition) (Digests, error) {
	if err := validateBriefLength(int64(len(d.Brief))); err != nil {
		return Digests{}, err
	}
	briefSHA256 := task.ComputeSHA256(d.Brief)
	definitionSHA256, err := HeaderSHA256(d.DefinitionHeader, briefSHA256)
	if err != nil {
		return Digests{}, err
	}
	return Digests{BriefSHA256: briefSHA256, DefinitionSHA256: definitionSHA256}, nil
}

// HeaderSHA256 validates the header and returns the definition digest that
// binds it to briefSHA256.
func HeaderSHA256(header DefinitionHeader, briefSHA256 string) (string, error) {
	if err := validateHeader(header); err != nil {
		return "", err
	}
	if err := task.ValidateSHA256(briefSHA256); err != nil {
		return "", fmt.Errorf("schedule brief digest: %w", err)
	}
	encoded, err := task.MarshalCanonical(definitionDigestRecord{
		DigestVersion:      definitionDigestVersion,
		ScheduleID:         header.ScheduleID,
		CreatedAtUnixNanos: header.CreatedAt.UnixNano(),
		Cron:               header.Cron,
		Timezone:           header.Timezone,
		Request:            header.Request,
		BriefSHA256:        briefSHA256,
	})
	if err != nil {
		return "", fmt.Errorf("schedule definition digest: %w", err)
	}
	return task.ComputeSHA256(encoded), nil
}

// VerifyDefinitionDigests recomputes both digests of a fully read definition
// and reports any difference from the stored values as an invariant fault.
func VerifyDefinitionDigests(d Definition, stored Digests) error {
	computed, err := DefinitionDigests(d)
	if err != nil {
		return err
	}
	if computed != stored {
		return fmt.Errorf("%w: schedule %s digests do not match its definition", task.ErrInvariantFault, d.ScheduleID)
	}
	return nil
}

func validateHeader(header DefinitionHeader) error {
	if err := validateScheduleID(header.ScheduleID); err != nil {
		return err
	}
	if err := ValidateInstant("schedule created_at", header.CreatedAt); err != nil {
		return err
	}
	if err := validateOpaqueText("schedule cron", header.Cron); err != nil {
		return err
	}
	if err := validateOpaqueText("schedule timezone", header.Timezone); err != nil {
		return err
	}
	_, err := encodeRequest(header.Request)
	return err
}

func validateBriefLength(length int64) error {
	if length < 1 || length > task.MaxBriefSize {
		return errors.New("schedule brief must be between 1 byte and 8 MiB")
	}
	return nil
}

func validateScheduleID(id string) error {
	if err := task.ValidateTaskID(id); err != nil {
		return fmt.Errorf("schedule id: %w", err)
	}
	return nil
}

func validateOpaqueText(name, value string) error {
	if value == "" || len(value) > MaxOpaqueTextBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be nonempty UTF-8 of at most %d bytes", name, MaxOpaqueTextBytes)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	return nil
}
