package verifiedexec

import "errors"

// ErrAdmittedExecutableChanged is decoded from historical guardian records.
// New installed commands never produce this retired binding outcome.
var ErrAdmittedExecutableChanged = errors.New("admitted executable changed after admission")
