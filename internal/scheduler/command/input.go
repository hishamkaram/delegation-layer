// Package command owns schedule definition and mutation decisions over scheduler contracts.
package command

import (
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

// OperationTimeout bounds a schedule command's storage work and uncertain-write readback.
const OperationTimeout = 10 * time.Second

// CreateInput owns one captured creation request. Recurrence resolution
// later supplies the stored timezone; requested options are not normalized.
type CreateInput struct {
	Root        string
	ID          string
	CreatedAt   time.Time
	Cron        string
	Timezone    string
	TimezoneSet bool
	Request     contract.Request
	Brief       []byte
}

var ErrInvalidInput = errors.New("invalid schedule request")

func InvalidInput(err error) error {
	return fmt.Errorf("%w: %w", ErrInvalidInput, err)
}

// Warning is a bounded schedule decision warning, independent of presentation.
type Warning struct {
	Code    string
	Message string
}
