package infrastructure

import (
	"strconv"
	"strings"
)

const ownerModeArgument = "--infrastructure-owner"

// ownerInvocation is routing/reference data only, never native birth or Start
// authority. Parsing performs no environment, filesystem, descriptor or process I/O.
type ownerInvocation struct {
	root    string
	rootID  string
	startup StartupReference
}

func (i ownerInvocation) validate() error {
	if _, err := NewOrigin(i.root, i.rootID); err != nil {
		return err
	}
	return i.startup.Validate()
}

// Recognition runs before validation so malformed or misplaced owner modes can
// never fall through to another runner mode.
func parseOwnerInvocation(args []string) (ownerInvocation, bool, error) {
	handled := false
	for _, argument := range args {
		if argument == ownerModeArgument || strings.HasPrefix(argument, ownerModeArgument+"=") {
			handled = true
			break
		}
	}
	if !handled {
		return ownerInvocation{}, false, nil
	}
	if len(args) != 11 || args[0] != ownerModeArgument {
		return ownerInvocation{}, true, ErrIdentity
	}
	invocation, err := parseOwnerValues(args[1:])
	if err != nil {
		return ownerInvocation{}, true, err
	}
	if err = invocation.validate(); err != nil {
		return ownerInvocation{}, true, err
	}
	return invocation, true, nil
}

func parseOwnerValues(args []string) (ownerInvocation, error) {
	var invocation ownerInvocation
	seen := make(map[string]bool, 5)
	for index := 0; index < len(args); index += 2 {
		key, value := args[index], args[index+1]
		if seen[key] || value == "" || strings.HasPrefix(value, "-") {
			return ownerInvocation{}, ErrIdentity
		}
		seen[key] = true
		switch key {
		case "--root":
			invocation.root = value
		case "--root-id":
			invocation.rootID = value
		case "--sequence":
			sequence, err := strconv.ParseUint(value, 10, 64)
			if err != nil || strconv.FormatUint(sequence, 10) != value {
				return ownerInvocation{}, ErrIdentity
			}
			invocation.startup.Sequence = sequence
		case "--epoch":
			invocation.startup.Epoch = value
		case "--intent-sha256":
			invocation.startup.IntentSHA256 = value
		default:
			return ownerInvocation{}, ErrIdentity
		}
	}
	return invocation, nil
}

// ownerArguments is the sole canonical argv encoder for this private mode.
func ownerArguments(root, rootID string, reference StartupReference) ([]string, error) {
	invocation := ownerInvocation{root: root, rootID: rootID, startup: reference}
	if err := invocation.validate(); err != nil {
		return nil, err
	}
	return []string{ownerModeArgument, "--root", root, "--root-id", rootID, "--sequence", strconv.FormatUint(reference.Sequence, 10), "--epoch", reference.Epoch, "--intent-sha256", reference.IntentSHA256}, nil
}
