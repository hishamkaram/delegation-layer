package pueue

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const (
	privateTaskObservationLockName   = "task-observation.lock"
	privateTaskObservationRecordName = "task-observation.ndjson"
	privateTaskObservationHeaderKind = "task-observation-header"
	privateTaskObservationJobKind    = "task-observation-job"
	privateTaskObservationFooterKind = "task-observation-footer"
)

var errPrivateTaskObservationInvalid = errors.New("invalid private task observation")

// privateTaskObservationHeader identifies one complete live queue snapshot.
// FlightID is a freshness token, not durable task state: only callers that
// entered before its publication may reuse the snapshot.
type privateTaskObservationHeader struct {
	SchemaVersion int                `json:"schema_version"`
	Kind          string             `json:"kind"`
	FlightID      string             `json:"flight_id"`
	Binding       task.SupervisorRef `json:"binding"`
	JobCount      int                `json:"job_count"`
}

type privateTaskObservationJob struct {
	Kind      string `json:"kind"`
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	State     State  `json:"state"`
	Group     string `json:"group"`
	Succeeded bool   `json:"succeeded"`
}

type privateTaskObservationFooter struct {
	Kind     string `json:"kind"`
	JobCount int    `json:"job_count"`
}

// PrivateTaskObservationReady reports whether a published private generation
// has the read-side coordination boundary required by ReconcileStatus. It is
// read-only; a missing lock is repaired only by the lifecycle transition.
func PrivateTaskObservationReady(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, ErrConfiguration
	}
	return privateLockFilePresent(filepath.Join(stateRoot, privateSupervisorDirectory), privateTaskObservationLockName)
}

// EnsurePrivateTaskObservationWithBootstrapLock provisions task-status
// coordination while the caller owns the private lifecycle transition.
func EnsurePrivateTaskObservationWithBootstrapLock(stateRoot string) error {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		return err
	}
	return ensurePrivateLockFile(base, privateTaskObservationLockName)
}

// ReconcileStatus shares one complete private queue observation among callers
// that overlap the same supervisor generation. Calls that begin after a flight
// was published perform a fresh observation. External supervisors retain the
// exact label-filtered Reconcile behavior.
func (c *Client) ReconcileStatus(ctx context.Context, identity Identity) (Observation, error) {
	identity = snapshotIdentity(identity)
	unknown := Observation{State: StateUnknown, Identity: identity}
	if c == nil {
		return unknown, ErrConfiguration
	}
	unknown.Binding = c.binding
	if ctx == nil {
		return unknown, context.Canceled
	}
	if err := validateIdentity(identity); err != nil {
		return unknown, err
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return c.Reconcile(ctx, identity)
	}
	entryFlightID, err := readPrivateTaskObservationFlightID(stateRoot, c.runtimeBinding)
	if err != nil {
		return unknown, err
	}
	if c.options.taskObservationEntryObserver != nil {
		c.options.taskObservationEntryObserver()
	}
	snapshot, err := c.observePrivateTaskObservation(ctx, stateRoot, entryFlightID)
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	return matchingObservation(snapshot.Jobs, identity, c.binding)
}

func (c *Client) observePrivateTaskObservation(ctx context.Context, stateRoot, entryFlightID string) (QueueSnapshot, error) {
	observationContext, cancel := context.WithTimeout(ctx, c.options.ObservationTimeout)
	defer cancel()
	var snapshot QueueSnapshot
	err := c.withPrivateSupervisorLease(observationContext, func(lockedContext context.Context) error {
		return withPublishedPrivateLock(lockedContext, stateRoot, privateTaskObservationLockName, unix.LOCK_EX, func() error {
			current, err := c.reconcilePrivateTaskObservationLocked(lockedContext, stateRoot, entryFlightID)
			snapshot = current
			return err
		})
	})
	return snapshot, err
}

func (c *Client) reconcilePrivateTaskObservationLocked(ctx context.Context, stateRoot, entryFlightID string) (QueueSnapshot, error) {
	if err := c.verify(ctx); err != nil {
		return QueueSnapshot{}, err
	}
	current, header, found, err := readPrivateTaskObservation(stateRoot, c.runtimeBinding)
	if err != nil {
		return QueueSnapshot{}, err
	}
	if found && header.FlightID != entryFlightID {
		return current, nil
	}
	current, err = c.snapshotAfterVerify(ctx)
	if err != nil {
		return QueueSnapshot{}, err
	}
	if err := publishPrivateTaskObservation(stateRoot, c.runtimeBinding, current); err != nil {
		return QueueSnapshot{}, err
	}
	return current, nil
}

func privateTaskObservationPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateTaskObservationRecordName)
}

func readPrivateTaskObservationFlightID(stateRoot string, binding task.SupervisorRef) (flightID string, resultErr error) {
	file, err := openRegular(privateTaskObservationPath(stateRoot))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	reader := bufio.NewReaderSize(file, MaxControlBytes+1)
	line, prefix, err := reader.ReadLine()
	if err != nil || prefix || !utf8.Valid(line) {
		return "", nil
	}
	var header privateTaskObservationHeader
	if err = task.DecodeStrict(line, &header); err != nil || validatePrivateTaskObservationHeader(header, binding) != nil {
		return "", nil
	}
	return header.FlightID, nil
}

func readPrivateTaskObservation(stateRoot string, binding task.SupervisorRef) (snapshot QueueSnapshot, header privateTaskObservationHeader, found bool, resultErr error) {
	file, err := openRegular(privateTaskObservationPath(stateRoot))
	if errors.Is(err, os.ErrNotExist) {
		return QueueSnapshot{}, header, false, nil
	}
	if err != nil {
		return QueueSnapshot{}, header, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	valid, err := privateTaskObservationHasFinalNewline(file)
	if err != nil {
		return QueueSnapshot{}, header, false, err
	}
	if !valid {
		return QueueSnapshot{}, header, false, nil
	}
	reader := bufio.NewReaderSize(file, MaxControlBytes+1)
	header, valid = readPrivateTaskObservationHeader(reader, binding)
	if !valid {
		return QueueSnapshot{}, privateTaskObservationHeader{}, false, nil
	}
	jobs, valid := readPrivateTaskObservationJobs(reader, header.JobCount)
	if !valid || !readPrivateTaskObservationFooter(reader, header.JobCount) {
		return QueueSnapshot{}, privateTaskObservationHeader{}, false, nil
	}
	return QueueSnapshot{Jobs: jobs}, header, true, nil
}

func readPrivateTaskObservationHeader(reader *bufio.Reader, binding task.SupervisorRef) (privateTaskObservationHeader, bool) {
	line, err := readPrivateTaskObservationLine(reader)
	if err != nil {
		return privateTaskObservationHeader{}, false
	}
	var header privateTaskObservationHeader
	if err := task.DecodeStrict(line, &header); err != nil {
		return privateTaskObservationHeader{}, false
	}
	if err := validatePrivateTaskObservationHeader(header, binding); err != nil {
		return privateTaskObservationHeader{}, false
	}
	return header, true
}

func readPrivateTaskObservationJobs(reader *bufio.Reader, count int) ([]Job, bool) {
	jobs := make([]Job, 0)
	seenIDs := make(map[int64]struct{})
	for range count {
		line, err := readPrivateTaskObservationLine(reader)
		if err != nil {
			return nil, false
		}
		var row privateTaskObservationJob
		if err := task.DecodeStrict(line, &row); err != nil {
			return nil, false
		}
		if err := validatePrivateTaskObservationJob(row); err != nil {
			return nil, false
		}
		if _, duplicate := seenIDs[row.ID]; duplicate {
			return nil, false
		}
		seenIDs[row.ID] = struct{}{}
		label := row.Label
		jobs = append(jobs, Job{ID: row.ID, Label: &label, State: row.State, Group: row.Group, Succeeded: row.Succeeded})
	}
	return jobs, true
}

func readPrivateTaskObservationFooter(reader *bufio.Reader, jobCount int) bool {
	line, err := readPrivateTaskObservationLine(reader)
	if err != nil {
		return false
	}
	var footer privateTaskObservationFooter
	if decodeErr := task.DecodeStrict(line, &footer); decodeErr != nil || footer.Kind != privateTaskObservationFooterKind || footer.JobCount != jobCount {
		return false
	}
	line, err = readPrivateTaskObservationLine(reader)
	return errors.Is(err, io.EOF) && len(line) == 0
}

func privateTaskObservationHasFinalNewline(file *os.File) (bool, error) {
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	var last [1]byte
	if _, err = file.ReadAt(last[:], info.Size()-1); err != nil {
		return false, err
	}
	return last[0] == '\n', nil
}

func readPrivateTaskObservationLine(reader *bufio.Reader) ([]byte, error) {
	line, prefix, err := reader.ReadLine()
	if prefix || len(line) > MaxControlBytes || !utf8.Valid(line) {
		return nil, errPrivateTaskObservationInvalid
	}
	return line, err
}

func validatePrivateTaskObservationHeader(header privateTaskObservationHeader, binding task.SupervisorRef) error {
	if header.SchemaVersion != task.SchemaVersion || header.Kind != privateTaskObservationHeaderKind || header.JobCount < 0 {
		return errPrivateTaskObservationInvalid
	}
	if err := task.ValidateTaskID(header.FlightID); err != nil {
		return errPrivateTaskObservationInvalid
	}
	if err := validatePrivateSupervisorRef(header.Binding); err != nil || !privateFullGenerationMatches(header.Binding, binding) {
		return errPrivateTaskObservationInvalid
	}
	return nil
}

func validatePrivateTaskObservationJob(row privateTaskObservationJob) error {
	if row.Kind != privateTaskObservationJobKind || row.ID < 0 || row.Label == "" {
		return errPrivateTaskObservationInvalid
	}
	switch row.State {
	case StateUnknown, StateQueued, StateStashed, StateRunning, StateEnded:
		return nil
	default:
		return errPrivateTaskObservationInvalid
	}
}

func publishPrivateTaskObservation(stateRoot string, binding task.SupervisorRef, snapshot QueueSnapshot) error {
	flightID, err := task.NewTaskID()
	if err != nil {
		return err
	}
	jobs := make([]Job, 0, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		if job.Label != nil {
			jobs = append(jobs, job)
		}
	}
	header := privateTaskObservationHeader{
		SchemaVersion: task.SchemaVersion,
		Kind:          privateTaskObservationHeaderKind,
		FlightID:      flightID,
		Binding:       binding,
		JobCount:      len(jobs),
	}
	var data bytes.Buffer
	if err = appendPrivateTaskObservationRecord(&data, header); err != nil {
		return err
	}
	for _, job := range jobs {
		row := privateTaskObservationJob{
			Kind:      privateTaskObservationJobKind,
			ID:        job.ID,
			Label:     *job.Label,
			State:     job.State,
			Group:     job.Group,
			Succeeded: job.Succeeded,
		}
		if err = appendPrivateTaskObservationRecord(&data, row); err != nil {
			return err
		}
	}
	if err = appendPrivateTaskObservationRecord(&data, privateTaskObservationFooter{Kind: privateTaskObservationFooterKind, JobCount: len(jobs)}); err != nil {
		return err
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	return replacePrivateRecordAtomic(base, privateTaskObservationPath(stateRoot), ".task-observation-", "private task observation", data.Bytes(), false)
}

func appendPrivateTaskObservationRecord[T any](destination *bytes.Buffer, record T) error {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return err
	}
	if len(data) > MaxControlBytes {
		return fmt.Errorf("%w: private task observation record", ErrControlLimit)
	}
	_, err = destination.Write(data)
	return err
}
