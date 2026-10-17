package protocol

import "errors"

const MaxControlBytes = 1 << 20

var (
	ErrConfiguration = errors.New("invalid pueue configuration")
	ErrBinding       = errors.New("pueue binding changed or unsupported")
	ErrUnknown       = errors.New("pueue observation is unknown")
	ErrControlLimit  = errors.New("pueue control output limit exceeded")
)
