package contract

import (
	"fmt"
	"strings"
)

func ValidateOwnedRefPrefix(prefix string) error {
	if prefix == "refs/delegate/" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("%w: ref deletion prefix must end with /", ErrUnownedRef)
	}
	base := strings.TrimSuffix(prefix, "/")
	if err := ValidateOwnedRef(base); err != nil {
		return err
	}
	return nil
}

func ValidateOwnedRef(ref string) error {
	if !strings.HasPrefix(ref, "refs/delegate/") || len(ref) <= len("refs/delegate/") {
		return fmt.Errorf("%w: %q", ErrUnownedRef, ref)
	}
	if invalidOwnedRefCharacters(ref) || invalidOwnedRefSyntax(ref) || invalidOwnedRefComponents(ref) {
		return fmt.Errorf("%w: invalid ref name %q", ErrUnownedRef, ref)
	}
	return nil
}

func invalidOwnedRefCharacters(ref string) bool {
	for _, character := range ref {
		if character <= 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func invalidOwnedRefSyntax(ref string) bool {
	return strings.ContainsAny(ref, "~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "//")
}

func invalidOwnedRefComponents(ref string) bool {
	for _, component := range strings.Split(ref, "/") {
		if component == "" || component == "." || component == ".." || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") || strings.HasSuffix(component, ".") {
			return true
		}
	}
	return false
}
