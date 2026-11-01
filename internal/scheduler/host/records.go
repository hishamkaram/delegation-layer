package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These immutable records contain identifiers only. H is the sole writer;
// latest generation is derived from canonical numbered epoch records.
type controllerKind string

const (
	managedController    controllerKind = ""
	foregroundController controllerKind = "foreground"
)

type epochRecord struct {
	Protocol int            `json:"protocol"`
	RootID   string         `json:"root_id"`
	Sequence uint64         `json:"sequence"`
	Epoch    string         `json:"epoch"`
	BuildID  string         `json:"build_id"`
	Host     Identity       `json:"host"`
	UID      int            `json:"uid"`
	Endpoint string         `json:"endpoint"`
	Kind     controllerKind `json:"kind,omitempty"`
}

type attemptRecord struct {
	RootID  string   `json:"root_id"`
	Epoch   string   `json:"epoch"`
	Attempt uint64   `json:"attempt"`
	Child   Identity `json:"child"`
}

type stopRecord struct {
	RootID         string   `json:"root_id"`
	Epoch          string   `json:"epoch"`
	Host           Identity `json:"host"`
	Attempt        uint64   `json:"attempt,omitempty"`
	Child          Identity `json:"child,omitempty"`
	ChildUncertain bool     `json:"child_uncertain,omitempty"`
}

// Only the H owner can publish this after joining its child and witnessing E
// free. It binds the immutable stop candidate, including uncertain child Start.
type closedRecord struct {
	Stop stopRecord `json:"stop"`
}

type foregroundDoneRecord struct {
	RootID string   `json:"root_id"`
	Epoch  string   `json:"epoch"`
	Host   Identity `json:"host"`
}

func epochName(sequence uint64) string { return fmt.Sprintf("host-%020d.json", sequence) }

func stopName(sequence uint64) string { return fmt.Sprintf("stop-%020d.json", sequence) }

func closedName(sequence uint64) string { return fmt.Sprintf("closed-%020d.json", sequence) }

func foregroundDoneName(sequence uint64) string { return fmt.Sprintf("finished-%020d.json", sequence) }

func attemptName(sequence, attempt uint64) string {
	return fmt.Sprintf("child-%020d-%020d.json", sequence, attempt)
}

func latestSequence(names []string) (uint64, error) {
	var latest uint64
	for _, name := range names {
		if !strings.HasPrefix(name, "host-") {
			continue
		}
		sequence, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "host-"), ".json"), 10, 64)
		if err != nil || sequence == 0 || name != epochName(sequence) {
			return 0, fmt.Errorf("%w: invalid host epoch record name", task.ErrEvidenceFault)
		}
		latest = max(latest, sequence)
	}
	return latest, nil
}

func (r epochRecord) validate(rootID string) error {
	if r.RootID != rootID || r.Sequence == 0 || r.Protocol <= 0 || r.BuildID == "" || r.UID != os.Geteuid() {
		return ErrIdentity
	}
	if err := r.Kind.validateEndpoint(r.Endpoint); err != nil {
		return err
	}
	if err := task.ValidateTaskID(r.Epoch); err != nil {
		return fmt.Errorf("%w: %w", ErrIdentity, err)
	}
	if r.Host.PID <= 0 || r.Host.StartToken == "" {
		return ErrIdentity
	}
	return nil
}

func (kind controllerKind) validateEndpoint(endpoint string) error {
	switch kind {
	case managedController:
		if endpoint == "" || !filepath.IsAbs(endpoint) || filepath.Base(endpoint) != "s" {
			return ErrIdentity
		}
	case foregroundController:
		if endpoint != "" {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	return nil
}
