package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ControlDir stores nonsecret inspection records outside ordinary tasks. It
// reuses Store's rooted access and durability algorithm. Close it before Store.
// The caller owns record schemas and must never supply native credential data.
type ControlDir struct {
	store *Store
	dir   string
	lock  *LockFile

	mu        sync.Mutex
	idle      *sync.Cond
	closeDone chan struct{}
	closeErr  error
	active    int
	closed    bool
}

// ControlTransaction is the scoped operation lease passed to WithLock. Its
// methods remain valid until the callback returns, including while Close is
// waiting for that callback. A transaction must not be retained after the
// callback returns.
type ControlTransaction struct {
	ctx  context.Context
	d    *ControlDir
	mu   sync.Mutex
	idle *sync.Cond
	open int
	done bool
}

const controlOperationTimeout = 5 * time.Second

// OpenInspection opens the one inspection operation associated with a task ID.
// It never creates an ordinary task or grants submission/start authority.
func (s *Store) OpenInspection(taskID string, create bool) (*ControlDir, error) {
	return s.OpenInspectionContext(context.Background(), taskID, create)
}

// OpenInspectionContext retains the caller deadline while acquiring maintenance.
func (s *Store) OpenInspectionContext(ctx context.Context, taskID string, create bool) (*ControlDir, error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return nil, err
	}
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "inspections", taskID), create)
}

// OpenInspectionGroup opens the root-scoped supervisor group journal. Its
// records, rather than its directory name, bind the selected daemon identity.
func (s *Store) OpenInspectionGroup(create bool) (*ControlDir, error) {
	return s.OpenInspectionGroupContext(context.Background(), create)
}

// OpenInspectionGroupContext preserves the caller deadline while opening the
// root-scoped supervisor group journal.
func (s *Store) OpenInspectionGroupContext(ctx context.Context, create bool) (*ControlDir, error) {
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "inspection-group"), create)
}

// OpenInspectionGroupGeneration opens an immutable successor journal for a
// changed supervisor identity. The legacy inspection-group directory remains
// untouched so its evidence can still be audited or recovered independently.
func (s *Store) OpenInspectionGroupGeneration(generation string, create bool) (*ControlDir, error) {
	return s.OpenInspectionGroupGenerationContext(context.Background(), generation, create)
}

// OpenInspectionGroupGenerationContext preserves the caller deadline while
// opening a supervisor-identity successor journal.
func (s *Store) OpenInspectionGroupGenerationContext(ctx context.Context, generation string, create bool) (*ControlDir, error) {
	if !validInspectionGroupGeneration(generation) {
		return nil, fmt.Errorf("%w: invalid inspection group generation", task.ErrEvidenceFault)
	}
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "inspection-group-"+generation), create)
}

// OpenRunnerRepair opens root-scoped receipts for completed managed-runner
// repair transitions. These receipts are evidence only; they grant neither
// task submission nor worker-start authority.
func (s *Store) OpenRunnerRepair(create bool) (*ControlDir, error) {
	return s.OpenRunnerRepairContext(context.Background(), create)
}

// OpenRunnerRepairContext preserves the caller deadline while opening the
// root-scoped managed-runner repair receipt directory.
func (s *Store) OpenRunnerRepairContext(ctx context.Context, create bool) (*ControlDir, error) {
	return s.openControlDirContext(ctx, filepath.Join(s.Root, "runner-repair"), create)
}

func (s *Store) openControlDirContext(ctx context.Context, path string, create bool) (result *ControlDir, err error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err = s.maintLock.LockSH(ctx); err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, s.maintLock.Unlock())
		if err != nil && result != nil {
			err = errors.Join(err, result.Close())
			result = nil
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if create {
		if err = s.mkdirContext(ctx, path, s.faultInjector); err != nil {
			return nil, err
		}
	}
	open := s.openExistingLock
	if create {
		open = s.openLock
	}
	lock, err := open(filepath.Join(path, ".operation.lock"), LockLevelAdmission)
	if err != nil {
		return nil, err
	}
	d := &ControlDir{store: s, dir: path, lock: lock, closeDone: make(chan struct{})}
	d.idle = sync.NewCond(&d.mu)
	return d, nil
}

// WithLock serializes a bounded record transaction. fn must not start commands
// or wait on external operations. No authority follows from acquiring this lock;
// submission and worker start each require a newly created immutable guard.
// Use the scoped transaction argument for all record operations in fn so Close
// can reject unrelated calls without interrupting this transaction.
func (d *ControlDir) WithLock(ctx context.Context, fn func(*ControlTransaction) error) (err error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err = d.begin(); err != nil {
		return err
	}
	defer d.end()
	tx := &ControlTransaction{d: d, ctx: ctx}
	tx.idle = sync.NewCond(&tx.mu)
	if fn == nil {
		return errors.New("missing control transaction")
	}
	if err = d.store.maintLock.LockSH(ctx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.store.maintLock.Unlock()) }()
	if err = d.lock.LockEX(ctx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.lock.Unlock()) }()
	// Drain scoped operations before releasing either lock. A callback may
	// hand a transaction method to a bounded helper goroutine; releasing the
	// operation lock first would let the next transaction overlap that write.
	defer tx.finish()
	return fn(tx)
}

// Put creates one bounded canonical record. An identical existing winner
// returns created=false; a conflicting winner is an evidence fault. Guard
// consumers must require created=true AND err=nil before an external action.
func (d *ControlDir) Put(name string, record any) (created bool, err error) {
	return d.PutContext(context.Background(), name, record)
}

// PutContext preserves the caller's lifetime while waiting for maintenance.
// Once staging starts, its durability and cleanup work remain joined.
func (d *ControlDir) PutContext(ctx context.Context, name string, record any) (created bool, err error) {
	if err = d.begin(); err != nil {
		return false, err
	}
	defer d.end()
	return d.put(ctx, name, record)
}

func (d *ControlDir) put(ctx context.Context, name string, record any) (created bool, err error) {
	if err = validateControlName(name); err != nil {
		return false, err
	}
	data, err := marshalControlRecord(record)
	if err != nil {
		return false, err
	}
	created, cleanupErr, commitErr := d.store.stageReaderContext(ctx, d.dir, name, bytes.NewReader(data), d.store.faultInjector)
	if errors.Is(commitErr, os.ErrExist) && !created {
		return false, errors.Join(cleanupErr, d.verifyCanonicalRecord(name, data))
	}
	return created, errors.Join(cleanupErr, commitErr)
}

func (d *ControlDir) verifyCanonicalRecord(name string, data []byte) error {
	existing, err := d.store.readBytes(filepath.Join(d.dir, name), int64(len(data)))
	if err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	if !bytes.Equal(existing, data) {
		return task.ErrEvidenceFault
	}
	// The original writer may have linked this winner before a failed
	// directory barrier. Reestablish that barrier before accepting it;
	// this still never grants a fresh guard to the caller.
	if err := d.store.barrierDirectoryEntry(d.dir, filepath.Dir(d.dir), d.store.faultInjector); err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	return nil
}

// Read strictly decodes a bounded record through the pinned root descriptor.
func (d *ControlDir) Read(name string, record any) error {
	if err := d.begin(); err != nil {
		return err
	}
	defer d.end()
	return d.read(name, record)
}

// RecordNamesContext lists names through the pinned root. The record-family
// owner decides which names are records and validates them with Read.
func (d *ControlDir) RecordNamesContext(ctx context.Context) (names []string, resultErr error) {
	if err := d.begin(); err != nil {
		return nil, err
	}
	defer d.end()
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := d.store.openDir(d.dir)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	names, err = directory.Readdirnames(-1)
	return names, errors.Join(err, ctx.Err())
}

func (d *ControlDir) read(name string, record any) error {
	if err := validateControlName(name); err != nil {
		return err
	}
	return d.store.readRecord(filepath.Join(d.dir, name), record)
}

func (tx *ControlTransaction) enter() error {
	if tx == nil {
		return os.ErrClosed
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done || tx.d == nil {
		return os.ErrClosed
	}
	tx.open++
	return nil
}

func (tx *ControlTransaction) leave() {
	tx.mu.Lock()
	tx.open--
	if tx.open == 0 && tx.idle != nil {
		tx.idle.Broadcast()
	}
	tx.mu.Unlock()
}

func (tx *ControlTransaction) finish() {
	tx.mu.Lock()
	tx.done = true
	for tx.open > 0 {
		tx.idle.Wait()
	}
	tx.mu.Unlock()
}

// Put creates a canonical record within the enclosing WithLock transaction.
func (tx *ControlTransaction) Put(name string, record any) (bool, error) {
	if err := tx.enter(); err != nil {
		return false, err
	}
	defer tx.leave()
	return tx.d.put(tx.ctx, name, record)
}

// Read decodes a record within the enclosing WithLock transaction.
func (tx *ControlTransaction) Read(name string, record any) error {
	if err := tx.enter(); err != nil {
		return err
	}
	defer tx.leave()
	return tx.d.read(name, record)
}

// Verify checks exact canonical bytes and durability of an existing record
// under the enclosing WithLock leases. It never creates a record or stage.
func (tx *ControlTransaction) Verify(name string, record any) error {
	if err := tx.enter(); err != nil {
		return err
	}
	defer tx.leave()
	if err := validateControlName(name); err != nil {
		return err
	}
	data, err := marshalControlRecord(record)
	if err != nil {
		return err
	}
	return tx.d.verifyCanonicalRecord(name, data)
}

func validateControlName(name string) error {
	if !strings.HasSuffix(name, ".json") || len(name) > 64 {
		return fmt.Errorf("%w: invalid control record name", task.ErrEvidenceFault)
	}
	stem := strings.TrimSuffix(name, ".json")
	if stem == "" {
		return fmt.Errorf("%w: invalid control record name", task.ErrEvidenceFault)
	}
	for _, char := range stem {
		if char != '-' && (char < '0' || char > '9') && (char < 'a' || char > 'z') {
			return fmt.Errorf("%w: invalid control record name", task.ErrEvidenceFault)
		}
	}
	return nil
}

func validInspectionGroupGeneration(generation string) bool {
	if len(generation) != 16 {
		return false
	}
	for _, char := range generation {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// persistenceContext makes taskdir the sole owner of local persistence timing.
// A finite caller deadline remains authoritative. Calls without one receive a
// bounded fallback so direct background use cannot wait forever. Cancellation
// bounds lock acquisition and pre-publication checks; once durable staging has
// started, its barriers and owned cleanup remain joined.
func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if _, ok := ctx.Deadline(); ok {
		bounded, cancel := context.WithCancel(ctx)
		return bounded, cancel, nil
	}
	bounded, cancel := context.WithTimeout(ctx, controlOperationTimeout)
	return bounded, cancel, nil
}

func (d *ControlDir) begin() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return os.ErrClosed
	}
	d.active++
	return nil
}

func (d *ControlDir) end() {
	d.mu.Lock()
	d.active--
	if d.active == 0 && d.idle != nil {
		d.idle.Broadcast()
	}
	d.mu.Unlock()
}

// Close waits for in-flight operations to release this handle, then releases
// the stable operation lock without unlinking it. Once Close begins, new
// operations fail before validating or writing any record.
func (d *ControlDir) Close() error {
	d.mu.Lock()
	if d.closeDone == nil {
		d.closeDone = make(chan struct{})
	}
	if d.closed {
		done := d.closeDone
		d.mu.Unlock()
		<-done
		d.mu.Lock()
		err := d.closeErr
		d.mu.Unlock()
		return err
	}
	d.closed = true
	for d.active > 0 {
		d.idle.Wait()
	}
	done := d.closeDone
	d.mu.Unlock()

	err := d.lock.Close()
	d.mu.Lock()
	d.closeErr = err
	close(done)
	d.mu.Unlock()
	return err
}
