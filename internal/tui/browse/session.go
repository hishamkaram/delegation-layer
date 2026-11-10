// Package browse owns the TUI's history handle and background reconciliation.
// The catalog owns its derived data; task stores remain authoritative. An
// initially absent source is observed again without initializing that source.
package browse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/history"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type Session struct {
	// Session alone owns this handle. Readers cannot obtain it while cleanup
	// is unresolved; Catalog alone owns its database and native leases.
	sourcePin *taskdir.RootPin
	mu        sync.Mutex
	options   history.Options
	catalog   *history.Catalog
	// closeCatalog is the private close boundary; production always selects
	// Catalog.Close. Tests can fail closure without mutating shared globals.
	closeCatalog func(*history.Catalog) error
	problem      error
	closePending bool
	resetting    bool
	cancelScan   context.CancelFunc
	wake         chan struct{}
}

func New(options history.Options) *Session {
	return &Session{options: options, closeCatalog: (*history.Catalog).Close, wake: make(chan struct{}, 1)}
}

func (s *Session) Catalog() (*history.Catalog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resetting {
		return nil, history.ErrCacheBusy
	}
	if s.closePending {
		return nil, s.problem
	}
	return s.catalog, s.problem
}

func (s *Session) open(ctx context.Context) error {
	if err := s.AssertBinding(ctx); err != nil {
		s.mu.Lock()
		if !s.closePending && !s.resetting {
			s.problem = err
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closePending {
		return s.problem
	}
	if s.catalog != nil || s.resetting {
		return nil
	}
	c, err := history.Open(ctx, s.options)
	s.catalog, s.problem = c, err
	s.closePending = c != nil && err != nil
	return err
}

func (s *Session) RequestRefresh() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Maintain has exactly one session-owned caller. At large history sizes the
// measured pass duration sets the cadence. Metadata discovery interleaves
// bounded observations so initial indexing cannot starve global status coverage.
func (s *Session) Maintain(ctx context.Context, report func(error)) error {
	ticker := time.NewTicker(history.DefaultRefreshInterval)
	defer ticker.Stop()
	var nextScan time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		var err error
		nextScan, err = s.maintainPass(ctx, nextScan)
		if err != nil && !expectedMaintenanceError(err) {
			report(err)
			if errors.Is(err, history.ErrRootChanged) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
			nextScan = time.Time{}
		case <-ticker.C:
		}
	}
}

// maintainPass schedules work for this session's one catalog owner. A scan
// already includes bounded observation progress; the regular batch continues
// coverage between scans without owning another writer or worker lifetime.
func (s *Session) maintainPass(ctx context.Context, nextScan time.Time) (time.Time, error) {
	if err := s.open(ctx); err != nil {
		if errors.Is(err, history.ErrRootChanged) {
			return nextScan, err
		}
		// Opening publishes cache/source unavailability through Catalog/Description;
		// exact-ID actions stay available and the next tick can observe first admission.
		return nextScan, nil
	}
	c, err := s.Catalog()
	if err != nil || c == nil {
		return nextScan, err
	}
	if !time.Now().Before(nextScan) {
		start := time.Now()
		err = s.scan(ctx, c)
		nextScan = time.Now().Add(max(history.DefaultScanInterval, 20*time.Since(start)))
		if err != nil {
			return nextScan, err
		}
	}
	_, err = c.RefreshObservations(ctx, history.DefaultObservationBatch)
	return nextScan, err
}

func expectedMaintenanceError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, history.ErrNotWriter) || errors.Is(err, history.ErrCacheBusy)
}

func (s *Session) scan(ctx context.Context, c *history.Catalog) error {
	scanContext, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.resetting {
		s.mu.Unlock()
		cancel()
		return history.ErrCacheBusy
	}
	if s.closePending {
		err := s.problem
		s.mu.Unlock()
		cancel()
		return err
	}
	s.cancelScan = cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.cancelScan = nil; s.mu.Unlock() }()
	_, err := c.ScanAndObserve(scanContext)
	return err
}

// Rebuild replaces disposable data only through Catalog's exclusive use lease.
// Existing source reads are cancelled/closed first; other sessions can refuse
// the rebuild without having any of their files removed.
func (s *Session) Rebuild(ctx context.Context) error {
	s.mu.Lock()
	if s.resetting {
		s.mu.Unlock()
		return history.ErrCacheBusy
	}
	s.resetting = true
	if s.cancelScan != nil {
		s.cancelScan()
	}
	old := s.catalog
	s.mu.Unlock()
	var err error
	if old != nil {
		err = s.closeCatalog(old)
	}
	s.mu.Lock()
	err = s.recordCatalogClose(err)
	s.mu.Unlock()
	if err == nil {
		options := s.options
		options.Rebuild = true
		var catalog *history.Catalog
		catalog, err = history.Open(ctx, options)
		s.mu.Lock()
		s.catalog = catalog
		s.closePending = catalog != nil && err != nil
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.problem = err
	s.resetting = false
	s.mu.Unlock()
	s.RequestRefresh()
	return err
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resetting {
		return history.ErrCacheBusy
	}
	if s.cancelScan != nil {
		s.cancelScan()
	}
	if s.catalog == nil {
		return nil
	}
	return s.recordCatalogClose(s.closeCatalog(s.catalog))
}

// recordCatalogClose runs under mu. A failure retains the exact handle and
// blocks reopening. Explicit retries can settle consumer failures; a latched
// physical database-close failure remains restart-only.
func (s *Session) recordCatalogClose(err error) error {
	s.closePending = err != nil
	if err != nil {
		s.problem = err
		if !errors.Is(err, history.ErrCloseUnresolved) {
			s.problem = fmt.Errorf("history cache cleanup pending; retry close or rebuild: %w", err)
		}
		return s.problem
	}
	s.catalog, s.problem = nil, nil
	return nil
}

func (s *Session) Description() string {
	c, err := s.Catalog()
	if err != nil {
		return fmt.Sprintf("History unavailable: %v. Exact-ID operations remain available.", err)
	}
	if c != nil && c.Rebuilt() {
		return "Disposable history cache rebuilt; indexing progress is shown below."
	}
	return ""
}

// AssertBinding refreshes the observed source identity before an effect. A
// missing initial source has no previously displayed task identity to retarget;
// its first admission remains validated/initialized by the application owner.
func (s *Session) AssertBinding(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, s.options.Root, s.options.Registry)
	s.mu.Lock()
	previous := s.sourcePin
	s.mu.Unlock()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if previous != nil {
			return errors.Join(history.ErrRootChanged, err)
		}
		if task.RecordAbsent(err) {
			return nil
		}
		return err
	}
	pin, pinErr := reader.PinRootContext(ctx)
	closeErr := reader.Close()
	if err = errors.Join(pinErr, closeErr); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sourcePin == nil {
		s.sourcePin = &pin
		return nil
	}
	if !s.sourcePin.Equal(pin) {
		return history.ErrRootChanged
	}
	return nil
}
