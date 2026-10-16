package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const gitOwnedRefFormat = "%(refname)%00%(objectname)%00%(objecttype)%00%(symref)%00"

var errRefAbsent = errors.New("owned Git ref is absent")

type gitExitCoder interface {
	ExitCode() int
}

// PinBase creates an immutable Delegate-owned ref pointing at the committed
// source base. The caller must persist its candidate before invoking this
// create-only operation. A matching ref from an ambiguous prior create is
// accepted; a conflicting ref is never overwritten.
func (g Git) PinBase(ctx context.Context, source Source, ref string) error {
	if ctx == nil {
		return errors.Join(ErrInvalidGitContext, errors.New("nil base pin context"))
	}
	if err := validateOwnedRef(ref); err != nil {
		return err
	}
	if err := g.validateSourceIdentity(ctx, source); err != nil {
		return err
	}
	return g.pinCreateOnly(ctx, source.Root, ref, source.BaseOID, source.ObjectFormat, "commit", "pin Git base ref")
}

// PinCapture creates an immutable Delegate-owned tree ref after the caller
// persists candidate.ObservedHEAD and candidate.TreeOID as its authoritative
// candidate record. A matching ref from an ambiguous prior create is accepted;
// a conflicting ref is never overwritten.
func (g Git) PinCapture(ctx context.Context, checkout Checkout, ref string, candidate CaptureCandidate) error {
	if ctx == nil {
		return errors.Join(ErrInvalidGitContext, errors.New("nil capture pin context"))
	}
	if err := validateOwnedRef(ref); err != nil {
		return err
	}
	if err := g.validateCheckout(ctx, checkout); err != nil {
		return err
	}
	if err := validateCaptureCandidate(candidate, checkout.Source); err != nil {
		return err
	}
	if err := g.validateCandidateObjects(ctx, checkout, candidate); err != nil {
		return err
	}
	return g.pinCreateOnly(ctx, checkout.Source.Root, ref, candidate.TreeOID, checkout.Source.ObjectFormat, "tree", "pin Git capture ref")
}

// DeleteRef deletes one Delegate-owned ref only when it still points at
// expectedOld. Git performs the compare-and-swap atomically.
func (g Git) DeleteRef(ctx context.Context, source Source, ref, expectedOld string) error {
	if err := validateOwnedRef(ref); err != nil {
		return err
	}
	prefix := ref[:strings.LastIndexByte(ref, '/')+1]
	return g.DeleteRefs(ctx, source, prefix, []ForgetRef{{Ref: ref, ExpectedOID: expectedOld}})
}

// DeleteRefs deletes the listed Delegate-owned refs in one native transaction.
// The caller's durable forget intent is authoritative for refs absent from the
// inventory; refs that are present must still match their expected OIDs.
func (g Git) DeleteRefs(ctx context.Context, source Source, prefix string, refs []ForgetRef) error {
	if ctx == nil {
		return errors.Join(ErrInvalidGitContext, errors.New("nil ref deletion context"))
	}
	if err := validateOwnedRefPrefix(prefix); err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	ordered, err := orderedRefDeletions(refs, prefix, source.ObjectFormat)
	if err != nil {
		return err
	}
	if err = g.validateSourceIdentity(ctx, source); err != nil {
		return err
	}
	inventory, err := g.listOwnedRefs(ctx, source.Root, prefix)
	if err != nil {
		return err
	}
	deletions, err := matchingRefDeletions(ordered, inventory, source.ObjectFormat)
	if err != nil || len(deletions) == 0 {
		return err
	}
	transaction := deleteRefsTransaction(deletions)
	_, stderr, err := g.runOutput(ctx, source.Root, []string{"update-ref", "--stdin", "-z", "--no-deref"}, nil, bytes.NewReader(transaction))
	if err != nil {
		return fmt.Errorf("delete Git refs transaction: %w", gitCommandError(err, stderr))
	}
	return nil
}

func orderedRefDeletions(refs []ForgetRef, prefix, objectFormat string) ([]ForgetRef, error) {
	ordered := append([]ForgetRef(nil), refs...)
	sort.Slice(ordered, func(first, second int) bool { return ordered[first].Ref < ordered[second].Ref })
	for index, reference := range ordered {
		if err := validateOwnedRef(reference.Ref); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(reference.Ref, prefix) {
			return nil, fmt.Errorf("%w: ref %q is outside deletion prefix %q", ErrUnownedRef, reference.Ref, prefix)
		}
		if err := validateOID(reference.ExpectedOID, objectFormat); err != nil {
			return nil, fmt.Errorf("invalid expected Git ref OID for %q: %w", reference.Ref, err)
		}
		if index > 0 && ordered[index-1].Ref == reference.Ref {
			return nil, fmt.Errorf("%w: duplicate expected Git ref %q", ErrIdentityMismatch, reference.Ref)
		}
	}
	return ordered, nil
}

func matchingRefDeletions(ordered []ForgetRef, inventory map[string]listedGitRef, objectFormat string) ([]ForgetRef, error) {
	deletions := make([]ForgetRef, 0, len(ordered))
	for _, reference := range ordered {
		listed, present := inventory[reference.Ref]
		if !present {
			continue
		}
		if listed.symref != "" {
			return nil, errors.Join(
				ErrRefUnreadable,
				fmt.Errorf("expected Git ref %q is symbolic and points to %q", reference.Ref, listed.symref),
			)
		}
		if listed.objectType == "" {
			return nil, fmt.Errorf("%w: expected Git ref %q has no object type", ErrRefUnreadable, reference.Ref)
		}
		if err := validateOID(listed.objectName, objectFormat); err != nil {
			return nil, errors.Join(
				ErrRefUnreadable,
				fmt.Errorf("expected Git ref %q has invalid object name: %w", reference.Ref, err),
			)
		}
		if !strings.EqualFold(listed.objectName, reference.ExpectedOID) {
			return nil, errors.Join(
				ErrRefConflict,
				fmt.Errorf("expected Git ref %q points at %s, want %s", reference.Ref, listed.objectName, reference.ExpectedOID),
			)
		}
		deletions = append(deletions, reference)
	}
	return deletions, nil
}

type listedGitRef struct {
	objectName string
	objectType string
	symref     string
}

func (g Git) listOwnedRefs(ctx context.Context, directory, prefix string) (map[string]listedGitRef, error) {
	var inventory map[string]listedGitRef
	stderr, err := g.readRecordOutput(ctx, directory, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, prefix}, func(input io.Reader) error {
		var parseErr error
		inventory, parseErr = parseOwnedRefInventoryReader(input)
		return parseErr
	})
	if err != nil {
		return nil, errors.Join(ErrRefUnreadable, err)
	}
	// Git may omit broken refs with a warning and still return exit zero. Such
	// an inventory cannot establish absence for an intentional deletion.
	if len(stderr) != 0 {
		return nil, fmt.Errorf("%w: Git ref inventory reported: %s", ErrRefUnreadable, removeOneTerminatingNewline(string(stderr)))
	}
	for ref := range inventory {
		if !strings.HasPrefix(ref, prefix) {
			return nil, fmt.Errorf("%w: Git inventory returned ref %q outside prefix %q", ErrRefUnreadable, ref, prefix)
		}
	}
	return inventory, nil
}

func parseOwnedRefInventory(data []byte) (map[string]listedGitRef, error) {
	return parseOwnedRefInventoryReader(bytes.NewReader(data))
}

func parseOwnedRefInventoryReader(input io.Reader) (map[string]listedGitRef, error) {
	reader := bufio.NewReader(input)
	inventory := make(map[string]listedGitRef)
	for {
		ref, err := readNULRecord(reader)
		if errors.Is(err, io.EOF) {
			return inventory, nil
		}
		if err != nil {
			return nil, err
		}
		fields := [3]string{}
		for index := range fields {
			field, readErr := readNULRecord(reader)
			if readErr != nil {
				return nil, readErr
			}
			fields[index] = string(field)
		}
		// for-each-ref appends LF after each formatted record, including when
		// its final format field is terminated with NUL.
		if separator, readErr := reader.ReadByte(); readErr != nil || separator != '\n' {
			return nil, errors.Join(errors.New("unterminated Git ref inventory record"), readErr)
		}
		refName := string(ref)
		if err := validateOwnedRef(refName); err != nil {
			return nil, err
		}
		if _, duplicate := inventory[refName]; duplicate {
			return nil, fmt.Errorf("duplicate Git ref inventory entry %q", refName)
		}
		inventory[refName] = listedGitRef{objectName: fields[0], objectType: fields[1], symref: fields[2]}
	}
}

func deleteRefsTransaction(refs []ForgetRef) []byte {
	transaction := make([]byte, 0, len(refs)*96+16)
	transaction = append(transaction, "start\x00"...)
	for _, reference := range refs {
		transaction = append(transaction, "delete "...)
		transaction = append(transaction, reference.Ref...)
		transaction = append(transaction, 0)
		transaction = append(transaction, reference.ExpectedOID...)
		transaction = append(transaction, 0)
	}
	transaction = append(transaction, "prepare\x00commit\x00"...)
	return transaction
}

func validateOwnedRefPrefix(prefix string) error {
	if prefix == "refs/delegate/" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("%w: ref deletion prefix must end with /", ErrUnownedRef)
	}
	base := strings.TrimSuffix(prefix, "/")
	if err := validateOwnedRef(base); err != nil {
		return err
	}
	return nil
}

func verifyRef(ctx context.Context, g Git, directory, ref, expected, objectFormat string) error {
	actual, err := g.lookupRef(ctx, directory, ref, objectFormat)
	if err != nil {
		return fmt.Errorf("verify Git ref: %w", err)
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%w: Git ref points at %s, want %s", ErrIdentityMismatch, actual, expected)
	}
	return nil
}

func (g Git) pinCreateOnly(ctx context.Context, directory, ref, objectID, objectFormat, objectType, operation string) error {
	if err := g.verifyObject(ctx, directory, objectID, objectFormat, objectType); err != nil {
		return fmt.Errorf("%s object: %w", operation, err)
	}
	zero := zeroOID(objectFormat)
	if err := g.run(ctx, directory, []string{"update-ref", "--no-deref", ref, objectID, zero}, nil, nil, nil, nil); err != nil {
		actual, inspectErr := g.lookupRef(ctx, directory, ref, objectFormat)
		switch {
		case inspectErr == nil && strings.EqualFold(actual, objectID):
			return nil
		case inspectErr == nil:
			return errors.Join(
				ErrRefConflict,
				fmt.Errorf("%s: existing Git ref points at %s, want %s", operation, actual, objectID),
				err,
			)
		case errors.Is(inspectErr, errRefAbsent):
			return fmt.Errorf("%s: %w", operation, err)
		default:
			return errors.Join(ErrRefUnreadable, fmt.Errorf("%s: %w", operation, err), inspectErr)
		}
	}
	return verifyRef(ctx, g, directory, ref, objectID, objectFormat)
}

func (g Git) lookupRef(ctx context.Context, directory, ref, objectFormat string) (string, error) {
	inventory, err := g.listOwnedRefs(ctx, directory, ref)
	if err != nil {
		return "", err
	}
	actual, present := inventory[ref]
	if !present {
		return "", errRefAbsent
	}
	if actual.symref != "" || actual.objectType == "" {
		return "", fmt.Errorf("%w: Git ref %q is symbolic or has no readable object", ErrRefUnreadable, ref)
	}
	if err := validateOID(actual.objectName, objectFormat); err != nil {
		return "", errors.Join(ErrRefUnreadable, fmt.Errorf("invalid Git ref object: %w", err))
	}
	return actual.objectName, nil
}

func (g Git) verifyObject(ctx context.Context, directory, objectID, objectFormat, objectType string) error {
	if err := validateOID(objectID, objectFormat); err != nil {
		return err
	}
	if objectType != "commit" && objectType != "tree" {
		return fmt.Errorf("unsupported Git object type %q", objectType)
	}
	actual, err := g.runScalar(ctx, directory, []string{"rev-parse", "--verify", "--end-of-options", objectID + "^{" + objectType + "}"}, nil)
	if err != nil {
		return err
	}
	if err := validateOID(actual, objectFormat); err != nil {
		return err
	}
	if !strings.EqualFold(actual, objectID) {
		return fmt.Errorf("%w: Git %s object resolves to %s, want %s", ErrIdentityMismatch, objectType, actual, objectID)
	}
	return nil
}

func (g Git) validateCandidateObjects(ctx context.Context, checkout Checkout, candidate CaptureCandidate) error {
	if err := g.verifyObject(ctx, checkout.Source.Root, candidate.BaseOID, checkout.Source.ObjectFormat, "commit"); err != nil {
		return fmt.Errorf("validate capture base object: %w", err)
	}
	if err := g.verifyObject(ctx, checkout.CheckoutPath, candidate.ObservedHEAD, checkout.Source.ObjectFormat, "commit"); err != nil {
		return fmt.Errorf("validate observed checkout HEAD object: %w", err)
	}
	if err := g.verifyObject(ctx, checkout.CheckoutPath, candidate.TreeOID, checkout.Source.ObjectFormat, "tree"); err != nil {
		return fmt.Errorf("validate capture tree object: %w", err)
	}
	return nil
}

func validateOwnedRef(ref string) error {
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

func zeroOID(objectFormat string) string {
	if objectFormat == "sha256" {
		return strings.Repeat("0", 64)
	}
	return strings.Repeat("0", 40)
}
