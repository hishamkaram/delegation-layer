package admission

import (
	"errors"
)

var errNoReadyProvider = errors.New("no provider is ready for the requested delegation")
