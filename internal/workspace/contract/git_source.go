package contract

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

func ValidateSourceShape(source Source) error {
	if !hasSourceFields(source) {
		return errors.Join(ErrInvalidGitContext, errors.New("incomplete Git source"))
	}
	if err := ValidateObjectFormat(source.ObjectFormat); err != nil {
		return err
	}
	if err := ValidateOID(source.BaseOID, source.ObjectFormat); err != nil {
		return fmt.Errorf("invalid source base OID: %w", err)
	}
	return ValidateSourceRelativeCwd(source.RelativeCwd)
}

func hasSourceFields(source Source) bool {
	return source.Root != "" && source.CommonDir != "" && source.GitDir != "" && source.BaseOID != "" && source.ObjectFormat != ""
}

func ValidateSourceRelativeCwd(value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return errors.Join(ErrInvalidGitContext, errors.New("invalid source relative cwd"))
	}
	relative := filepath.FromSlash(value)
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Clean(relative) != relative {
		return errors.Join(ErrInvalidGitContext, errors.New("invalid source relative cwd"))
	}
	return nil
}

func PathWithin(root, path string) bool {
	if root == path {
		return true
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func ValidateObjectFormat(format string) error {
	if format != "sha1" && format != "sha256" {
		return fmt.Errorf("%w: %q", ErrUnsupportedObjectFormat, format)
	}
	return nil
}

func ValidateOID(oid, objectFormat string) error {
	if err := ValidateObjectFormat(objectFormat); err != nil {
		return err
	}
	want := 40
	if objectFormat == "sha256" {
		want = 64
	}
	if len(oid) != want {
		return fmt.Errorf("invalid %s OID length %d", objectFormat, len(oid))
	}
	for _, character := range oid {
		if !isHexDigit(character) {
			return fmt.Errorf("invalid %s OID", objectFormat)
		}
	}
	return nil
}

func isHexDigit(character rune) bool {
	return (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')
}

func ValidateRevision(revision string) error {
	if revision == "" || strings.ContainsAny(revision, "\x00\r\n") {
		return errors.Join(ErrInvalidGitContext, errors.New("invalid Git revision"))
	}
	return nil
}
