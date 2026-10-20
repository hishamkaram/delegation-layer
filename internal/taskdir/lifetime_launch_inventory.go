package taskdir

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const lifetimeLaunchSequenceDigits = 20

// lifetimeLaunchNaming is one store's launch record file name shape:
// prefix, a 20-digit zero-padded sequence, then suffix.
type lifetimeLaunchNaming struct {
	prefix string
	suffix string
}

func taskLifetimeLaunchNaming() lifetimeLaunchNaming {
	return lifetimeLaunchNaming{suffix: lifetimeLaunchSuffix}
}

func controlLifetimeLaunchNaming() lifetimeLaunchNaming {
	return lifetimeLaunchNaming{prefix: controlLifetimeLaunchPrefix, suffix: controlLifetimeLaunchSuffix}
}

// parse reports whether name has this store's launch shape and, if so, its
// sequence. A launch-shaped name that is not a canonical sequence, or whose
// sequence lies outside the shared namespace, is refused rather than ignored.
func (n lifetimeLaunchNaming) parse(name string) (uint64, bool, error) {
	if !strings.HasPrefix(name, n.prefix) || !strings.HasSuffix(name, n.suffix) || len(name) < len(n.prefix)+len(n.suffix) {
		return 0, false, nil
	}
	digits := name[len(n.prefix) : len(name)-len(n.suffix)]
	sequence, err := strconv.ParseUint(digits, 10, 64)
	if len(digits) != lifetimeLaunchSequenceDigits || err != nil {
		return 0, true, fmt.Errorf("%w: malformed lifetime launch record name %q", task.ErrIdentityMismatch, name)
	}
	if err = task.ValidateLifetimeLaunchSequence(sequence); err != nil {
		return 0, true, err
	}
	return sequence, true, nil
}

// readLifetimeLaunchInventory streams one rooted directory a name at a time
// and returns the sorted launch sequences it holds. The shared count is
// enforced while enumerating, before any record is read; non-launch entries
// are skipped without being retained.
func (s *Store) readLifetimeLaunchInventory(ctx context.Context, path string, naming lifetimeLaunchNaming) ([]uint64, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	dir, err := s.openDir(path)
	if err != nil {
		return nil, err
	}
	return scanLifetimeLaunchInventory(ctx, dir, naming)
}

// scanLifetimeLaunchInventory collects launch sequences from an opened
// directory, which the shared scanner closes.
func scanLifetimeLaunchInventory(ctx context.Context, dir directoryNames, naming lifetimeLaunchNaming) ([]uint64, error) {
	inventory := lifetimeLaunchInventory{naming: naming}
	canceled := func() error { return lifetimeContextErr(ctx) }
	if err := scanDirectoryNames(canceled, dir, inventory.visit); err != nil {
		return nil, err
	}
	slices.Sort(inventory.sequences)
	return inventory.sequences, nil
}

// lifetimeLaunchInventory retains only launch names.
type lifetimeLaunchInventory struct {
	naming    lifetimeLaunchNaming
	sequences []uint64
}

// visit refuses a malformed name or a count beyond the shared namespace as
// soon as it is seen.
func (i *lifetimeLaunchInventory) visit(name string) error {
	sequence, launch, err := i.naming.parse(name)
	if err != nil || !launch {
		return err
	}
	if uint64(len(i.sequences)) >= task.MaxLifetimeLaunches {
		return fmt.Errorf("%w: more than %d lifetime launches", task.ErrEvidenceFault, task.MaxLifetimeLaunches)
	}
	i.sequences = append(i.sequences, sequence)
	return nil
}

// lifetimeLaunchSet is one reader's complete contiguous launch set and the
// local capacity those persisted winners use. It is derived per read and never
// stored.
type lifetimeLaunchSet struct {
	records  []task.LifetimeLaunchRecord
	capacity task.LifetimeLaunchCapacity
}

// add counts a verified record before appending it.
func (s *lifetimeLaunchSet) add(record *task.LifetimeLaunchRecord) error {
	if err := s.capacity.Add(record); err != nil {
		return err
	}
	s.records = append(s.records, *record)
	return nil
}

// requireContiguousLifetimeLaunches admits only the complete set 1..N. A gap,
// including one followed by later records, is an identity mismatch.
func requireContiguousLifetimeLaunches(sequences []uint64) error {
	for index, sequence := range sequences {
		if sequence != uint64(index+1) {
			return fmt.Errorf("%w: lifetime launch sequence %d is not contiguous", task.ErrIdentityMismatch, sequence)
		}
	}
	return nil
}

// lifetimeLaunchSequences inventories this operation's launch records under
// the enclosing WithLock leases.
func (tx *ControlTransaction) lifetimeLaunchSequences() ([]uint64, error) {
	if err := tx.enter(); err != nil {
		return nil, err
	}
	defer tx.leave()
	return tx.d.store.readLifetimeLaunchInventory(tx.ctx, tx.d.dir, controlLifetimeLaunchNaming())
}

// inventoriedLifetimeLaunchReadError keeps a listed record that cannot be read
// from ever reading as absent.
func inventoriedLifetimeLaunchReadError(name string, err error) error {
	if task.RecordAbsent(err) {
		return fmt.Errorf("%w: inventoried lifetime launch %s is missing: %w", task.ErrEvidenceFault, name, err)
	}
	return controlLifetimeReadError(name, err)
}
