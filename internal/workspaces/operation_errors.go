package workspaces

import (
	"errors"
)

var ErrOperationResultUnavailable = errors.New("workspace operation ended without a result")

var ErrCaptureUnavailable = errors.New("workspace capture is unavailable; checkout retained")
