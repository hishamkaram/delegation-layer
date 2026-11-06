package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This passive view owns no serving or process authority. Only stop reads this
// stable common projection; ordinary startup readers retain strict schemas.
type ownerStopLocator struct {
	pin               taskdir.RootPin
	reference         StartupReference
	assumption        StartupAssumption
	publication       OwnerPublication
	publicationSHA256 string
	bootID            string
	servingReleased   bool
}

type stopIntentRecord struct {
	Sequence uint64          `json:"sequence"`
	Epoch    string          `json:"epoch"`
	Inputs   json.RawMessage `json:"inputs"`
}

type stopRootInputs struct {
	Root       string `json:"root"`
	RootID     string `json:"root_id"`
	RootDevice uint64 `json:"root_device"`
	RootInode  uint64 `json:"root_inode"`
	BootID     string `json:"boot_id"`
}

type stopAssumptionRecord struct {
	Startup json.RawMessage `json:"startup"`
	Process json.RawMessage `json:"process"`
	UID     int             `json:"uid"`
}

type stopPublicationRecord struct {
	Protocol         json.RawMessage `json:"protocol"`
	Startup          json.RawMessage `json:"startup"`
	AssumptionSHA256 string          `json:"assumption_sha256"`
	Endpoint         json.RawMessage `json:"endpoint"`
}

type stopSettlementRecord struct {
	Protocol          json.RawMessage         `json:"protocol"`
	Startup           json.RawMessage         `json:"startup"`
	AssumptionSHA256  string                  `json:"assumption_sha256"`
	PublicationSHA256 string                  `json:"publication_sha256"`
	ObservedBootID    string                  `json:"observed_boot_id"`
	SettledAt         string                  `json:"settled_at"`
	Reason            startupSettlementReason `json:"reason"`
}

// Unknown fields are deliberately ignored only here. The closed set of stop
// projections requires exact spelling and nonnull known fields, while checking
// duplicate aliases and bounded JSON structure before decoding any projection.
func decodeOwnerStopObject[T stopIntentRecord | stopRootInputs | stopAssumptionRecord | stopPublicationRecord | stopSettlementRecord | StartupReference | ProcessIdentity | EndpointIdentity](data []byte, required ...string) (record T, resultErr error) {
	if err := task.ValidateJSONStructure(data); err != nil {
		return record, errors.Join(ErrIdentity, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return record, errors.Join(ErrIdentity, err)
	}
	for _, name := range required {
		value, present := fields[name]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return record, ErrIdentity
		}
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, errors.Join(ErrIdentity, err)
	}
	return record, nil
}

func readOwnerStopLocator(ctx context.Context, reader *taskdir.ReadOnlyStore) (ownerStopLocator, bool, error) {
	if ctx == nil || reader == nil {
		return ownerStopLocator{}, false, os.ErrInvalid
	}
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		return ownerStopLocator{}, false, err
	}
	sequence, err := latestOwnerStopSequence(ctx, reader)
	if err != nil || sequence == 0 {
		return ownerStopLocator{}, false, err
	}
	snapshot, err := readOwnerStopSnapshot(ctx, reader, pin, sequence)
	if err != nil {
		return ownerStopLocator{}, true, err
	}
	if err = verifyOwnerStopSnapshot(ctx, reader, snapshot.locator, snapshot.names, snapshot.records); err != nil {
		return ownerStopLocator{}, true, err
	}
	return snapshot.locator, true, nil
}

type ownerStopSnapshot struct {
	locator ownerStopLocator
	names   [4]string
	records [4][]byte
}

// Exact historical observation shares every raw record and root check with
// latest-owner selection, but a later accepted intent is not a mutation of
// these immutable records and cannot change the subject of a stop request.
func readOwnerStopSequence(ctx context.Context, reader *taskdir.ReadOnlyStore, pin taskdir.RootPin, sequence uint64) (ownerStopLocator, error) {
	snapshot, err := readOwnerStopSnapshot(ctx, reader, pin, sequence)
	if err != nil {
		return ownerStopLocator{}, err
	}
	if err = verifySelectedOwnerStopSnapshot(ctx, reader, snapshot.locator, snapshot.names, snapshot.records); err != nil {
		return ownerStopLocator{}, err
	}
	return snapshot.locator, nil
}

func readOwnerStopSnapshot(ctx context.Context, reader *taskdir.ReadOnlyStore, pin taskdir.RootPin, sequence uint64) (ownerStopSnapshot, error) {
	if ctx == nil || reader == nil {
		return ownerStopSnapshot{}, os.ErrInvalid
	}
	names := [4]string{intentName(sequence), assumptionName(sequence), publicationName(sequence), settlementName(sequence)}
	var records [4][]byte
	for index, name := range names {
		data, err := reader.ReadInfrastructureRecordContext(ctx, name)
		if index == 3 && task.RecordAbsent(err) {
			continue
		}
		if err != nil {
			return ownerStopSnapshot{}, errors.Join(task.ErrEvidenceFault, err)
		}
		if len(data) == 0 {
			return ownerStopSnapshot{}, errors.Join(task.ErrEvidenceFault, ErrIdentity)
		}
		records[index] = data
	}
	locator, err := ownerStopProjection(pin, sequence, records)
	if err != nil {
		return ownerStopSnapshot{}, err
	}
	return ownerStopSnapshot{locator: locator, names: names, records: records}, nil
}

func latestOwnerStopSequence(ctx context.Context, reader *taskdir.ReadOnlyStore) (uint64, error) {
	names, err := reader.InfrastructureRecordNamesContext(ctx)
	if task.RecordAbsent(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var latest uint64
	for _, name := range names {
		if !strings.HasPrefix(name, "intent-") {
			continue
		}
		sequence, parseErr := intentSequence(name)
		if parseErr != nil {
			return 0, parseErr
		}
		latest = max(latest, sequence)
	}
	return latest, nil
}

func ownerStopProjection(pin taskdir.RootPin, sequence uint64, records [4][]byte) (ownerStopLocator, error) {
	reference, bootID, err := decodeOwnerStopIntent(pin, sequence, records[0])
	if err != nil {
		return ownerStopLocator{}, err
	}
	assumption, err := decodeOwnerStopAssumption(records[1], reference)
	if err != nil {
		return ownerStopLocator{}, err
	}
	publication, err := decodeOwnerStopPublication(records[2], reference, pin, task.ComputeSHA256(records[1]))
	if err != nil {
		return ownerStopLocator{}, err
	}
	locator := ownerStopLocator{pin: pin, reference: reference, assumption: assumption, publication: publication, publicationSHA256: task.ComputeSHA256(records[2]), bootID: bootID}
	if len(records[3]) != 0 {
		locator.servingReleased, err = ownerStopReleased(records[3], locator)
	}
	return locator, err
}

func decodeOwnerStopIntent(pin taskdir.RootPin, sequence uint64, data []byte) (StartupReference, string, error) {
	intent, err := decodeOwnerStopObject[stopIntentRecord](data, "sequence", "epoch", "inputs")
	if err != nil {
		return StartupReference{}, "", err
	}
	inputs, err := decodeOwnerStopObject[stopRootInputs](intent.Inputs, "root", "root_id", "root_device", "root_inode", "boot_id")
	if err != nil {
		return StartupReference{}, "", err
	}
	reference := StartupReference{Sequence: sequence, Epoch: intent.Epoch, IntentSHA256: task.ComputeSHA256(data)}
	if err = reference.Validate(); err != nil || intent.Sequence != sequence || inputs.Root != pin.Root() || inputs.RootID != pin.RootID() || inputs.RootDevice != pin.Device() || inputs.RootInode != pin.Inode() || lifetime.CompareBootIDs(inputs.BootID, inputs.BootID) != lifetime.BootSame {
		return StartupReference{}, "", errors.Join(ErrIdentity, err)
	}
	return reference, inputs.BootID, nil
}

// A successor may still be unassumed. Its accepted intent must nevertheless
// bind this root and boot; a filename or partial record is never that proof.
func ownerStopSuccessorAccepted(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator) (bool, error) {
	latest, err := latestOwnerStopSequence(ctx, reader)
	if err != nil {
		return false, err
	}
	if latest == selected.reference.Sequence {
		return false, nil
	}
	if latest < selected.reference.Sequence {
		return false, ErrIdentity
	}
	data, err := reader.ReadInfrastructureRecordContext(ctx, intentName(latest))
	if err != nil {
		return false, errors.Join(task.ErrEvidenceFault, err)
	}
	_, bootID, err := decodeOwnerStopIntent(selected.pin, latest, data)
	if err != nil || lifetime.CompareBootIDs(selected.bootID, bootID) != lifetime.BootSame {
		return false, errors.Join(ErrIdentity, err)
	}
	current, err := reader.ReadInfrastructureRecordContext(ctx, intentName(latest))
	if err != nil || !bytes.Equal(current, data) {
		return false, errors.Join(ErrIdentity, err)
	}
	if err = requireContactPin(ctx, reader, selected.pin); err != nil {
		return false, err
	}
	return true, nil
}

func ownerStopReference(data []byte, expected StartupReference) error {
	reference, err := decodeOwnerStopObject[StartupReference](data, "sequence", "epoch", "intent_sha256")
	if err != nil || reference != expected {
		return errors.Join(ErrIdentity, err)
	}
	return nil
}

func decodeOwnerStopAssumption(data []byte, reference StartupReference) (StartupAssumption, error) {
	record, err := decodeOwnerStopObject[stopAssumptionRecord](data, "startup", "process", "uid")
	if err != nil {
		return StartupAssumption{}, err
	}
	process, err := decodeOwnerStopObject[ProcessIdentity](record.Process, "pid", "start_token")
	if err != nil {
		return StartupAssumption{}, err
	}
	_, processErr := process.Reference()
	if err = errors.Join(ownerStopReference(record.Startup, reference), processErr); err != nil || record.UID != os.Geteuid() {
		return StartupAssumption{}, errors.Join(ErrIdentity, err)
	}
	return StartupAssumption{Startup: reference, Process: process, UID: record.UID}, nil
}

func decodeOwnerStopPublication(data []byte, reference StartupReference, pin taskdir.RootPin, assumptionDigest string) (OwnerPublication, error) {
	record, err := decodeOwnerStopObject[stopPublicationRecord](data, "startup", "assumption_sha256", "endpoint")
	if err != nil {
		return OwnerPublication{}, err
	}
	endpoint, err := decodeOwnerStopObject[EndpointIdentity](record.Endpoint, "path", "directory_device", "directory_inode", "socket_device", "socket_inode")
	if err != nil {
		return OwnerPublication{}, err
	}
	protocol, err := ownerStopRecordProtocol(record.Protocol)
	if err != nil {
		return OwnerPublication{}, err
	}
	if err = ownerStopReference(record.Startup, reference); err != nil || record.AssumptionSHA256 != assumptionDigest || endpoint.Path != ownerEndpointPath(pin.Root(), reference.Sequence) || endpoint.DirectoryDev != pin.Device() || endpoint.DirectoryInode != pin.Inode() || endpoint.SocketInode == 0 {
		return OwnerPublication{}, errors.Join(ErrIdentity, err)
	}
	return OwnerPublication{Protocol: protocol, Startup: reference, AssumptionSHA256: assumptionDigest, Endpoint: endpoint}, nil
}

func ownerStopRecordProtocol(data json.RawMessage) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	var protocol int
	if err := json.Unmarshal(data, &protocol); err != nil || protocol <= 0 {
		return 0, errors.Join(ErrIdentity, err)
	}
	return protocol, nil
}

func ownerStopReleased(data []byte, locator ownerStopLocator) (bool, error) {
	record, err := decodeOwnerStopObject[stopSettlementRecord](data, "startup", "assumption_sha256", "publication_sha256", "observed_boot_id", "settled_at", "reason")
	if err != nil {
		return false, err
	}
	if _, err = ownerStopRecordProtocol(record.Protocol); err != nil {
		return false, err
	}
	if err = ownerStopReference(record.Startup, locator.reference); err != nil || record.AssumptionSHA256 != locator.publication.AssumptionSHA256 || record.PublicationSHA256 != locator.publicationSHA256 {
		return false, errors.Join(ErrIdentity, err)
	}
	if _, err = canonicalStartupTime(record.SettledAt); err != nil {
		return false, err
	}
	relation := lifetime.CompareBootIDs(locator.bootID, record.ObservedBootID)
	if record.Reason == settlementPreviousBoot && relation == lifetime.BootDifferent || record.Reason == settlementAssumedAbsent && relation == lifetime.BootSame {
		return false, nil
	}
	if record.Reason != settlementServingReleased || relation != lifetime.BootSame {
		return false, ErrIdentity
	}
	return true, nil
}

func verifyOwnerStopSnapshot(ctx context.Context, reader *taskdir.ReadOnlyStore, locator ownerStopLocator, names [4]string, records [4][]byte) error {
	if err := verifySelectedOwnerStopSnapshot(ctx, reader, locator, names, records); err != nil {
		return err
	}
	sequence, err := latestOwnerStopSequence(ctx, reader)
	if err != nil || sequence != locator.reference.Sequence {
		return errors.Join(ErrIdentity, err)
	}
	pin, err := reader.PinRootContext(ctx)
	if err != nil || !pin.Equal(locator.pin) {
		return errors.Join(ErrIdentity, err)
	}
	return ctx.Err()
}

func verifySelectedOwnerStopSnapshot(ctx context.Context, reader *taskdir.ReadOnlyStore, locator ownerStopLocator, names [4]string, records [4][]byte) error {
	for index, name := range names {
		current, readErr := reader.ReadInfrastructureRecordContext(ctx, name)
		if index == 3 && len(records[index]) == 0 {
			if task.RecordAbsent(readErr) {
				continue
			}
			if readErr != nil {
				return errors.Join(ErrIdentity, readErr)
			}
			released, releaseErr := ownerStopReleased(current, locator)
			if releaseErr != nil || !released {
				return errors.Join(ErrIdentity, releaseErr)
			}
			continue
		}
		if readErr != nil || !bytes.Equal(current, records[index]) {
			return errors.Join(ErrIdentity, readErr)
		}
	}
	pin, err := reader.PinRootContext(ctx)
	if err != nil || !pin.Equal(locator.pin) {
		return errors.Join(ErrIdentity, err)
	}
	return ctx.Err()
}
