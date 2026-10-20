package task

import "fmt"

// MaxLifetimeLaunchBytes bounds the summed canonical bytes of one task's
// individual launch records. It is independent of MaxLifetimeLaunches: a full
// count of small records and a short set of large records are both refused
// once either limit is reached. Every existing set of at most 64 records of at
// most MaxControlRecordSize each remains within it. It bounds encoded record
// bytes, not process memory.
const MaxLifetimeLaunchBytes = 64 * MaxControlRecordSize

type lifetimeLaunchLimits struct {
	count uint64
	bytes int
}

func defaultLifetimeLaunchLimits() lifetimeLaunchLimits {
	return lifetimeLaunchLimits{count: MaxLifetimeLaunches, bytes: MaxLifetimeLaunchBytes}
}

// LifetimeLaunchCapacity accounts one launch set, record by record, against
// the shared count and canonical-byte limits. Readers add each record after
// their store validates it and before appending it (control stores also
// verify its exact canonical bytes; task directories strictly decode it and
// count its canonical re-encoding); writers add the candidate before
// publishing it. The zero value enforces the exported limits. It is a local
// accumulator, never durable state.
type LifetimeLaunchCapacity struct {
	count  uint64
	bytes  int
	limits *lifetimeLaunchLimits
}

// Add accounts one record by its canonical encoding. A refused record leaves
// the accumulator unchanged.
func (c *LifetimeLaunchCapacity) Add(record *LifetimeLaunchRecord) error {
	data, err := MarshalCanonical(record)
	if err != nil {
		return err
	}
	return c.addEncoded(len(data))
}

func (c *LifetimeLaunchCapacity) addEncoded(size int) error {
	limits := defaultLifetimeLaunchLimits()
	if c.limits != nil {
		limits = *c.limits
	}
	if size > MaxControlRecordSize {
		return fmt.Errorf("%w: lifetime launch record exceeds %d canonical bytes", ErrEvidenceFault, MaxControlRecordSize)
	}
	if c.count >= limits.count {
		return fmt.Errorf("%w: more than %d lifetime launches", ErrEvidenceFault, limits.count)
	}
	if size > limits.bytes-c.bytes {
		return fmt.Errorf("%w: lifetime launch records exceed %d canonical bytes", ErrEvidenceFault, limits.bytes)
	}
	c.count++
	c.bytes += size
	return nil
}

// ValidateLifetimeLaunchSequence admits only a sequence inside the shared
// create-once launch namespace.
func ValidateLifetimeLaunchSequence(sequence uint64) error {
	if sequence == 0 || sequence > MaxLifetimeLaunches {
		return fmt.Errorf("%w: lifetime launch sequence is outside 1..%d", ErrEvidenceFault, MaxLifetimeLaunches)
	}
	return nil
}

// ValidateLifetimeLaunchCapacity checks an assembled launch set. Store
// writers instead add a new candidate to the capacity their set reader
// already accumulated, so existing records are encoded once per write.
func ValidateLifetimeLaunchCapacity(launches []LifetimeLaunchRecord) error {
	var capacity LifetimeLaunchCapacity
	for index := range launches {
		if err := capacity.Add(&launches[index]); err != nil {
			return err
		}
	}
	return nil
}

// LifetimeLaunchSetDigest is the canonical digest of a complete launch set
// bound by a termination record. A nil set digests as an empty array.
func LifetimeLaunchSetDigest(launches []LifetimeLaunchRecord) (string, error) {
	if launches == nil {
		launches = make([]LifetimeLaunchRecord, 0)
	}
	data, err := MarshalCanonical(launches)
	if err != nil {
		return "", err
	}
	return ComputeSHA256(data), nil
}
