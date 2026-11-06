package infrastructure

import (
	"context"
	"net"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/localpeer"
)

// These helpers authenticate only a connected peer and immutable record subject.
// Callers retain the actual root pin, validate current endpoint/owner selection,
// and own request deadlines and operation policy. Records alone mint no grant.
// In particular, a resident outlives its original assumption/startup deadline.
func authenticateOwnerRequest(ctx context.Context, conn *net.UnixConn, intent StartupIntent, assumption StartupAssumption, publication OwnerPublication, request ownerRequest) (resultErr error) {
	defer redactOwnerAuthenticationError(&resultErr)
	if err := ownerAuthenticationContext(ctx); err != nil {
		return err
	}
	digest, err := publication.Digest(intent, assumption)
	if err != nil {
		return err
	}
	if err = validateOwnerRequestSubject(intent, digest, request); err != nil {
		return err
	}
	if err = validateOwnerAuthenticationBoot(ctx, intent); err != nil {
		return err
	}
	peer, err := localpeer.Read(conn)
	if err != nil {
		return err
	}
	if peer.UID != os.Geteuid() || peer.UID != assumption.UID || peer.PID != request.Client.PID {
		return ErrIdentity
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// This endpoint is the local resident: unlike the remote-server client check,
	// the saved owner must be this exact native self incarnation.
	if err = validateSelfAssumption(assumption); err != nil {
		return err
	}
	return requireOwnerProcessPresent(ctx, request.Client)
}

func authenticateOwnerPeer(ctx context.Context, conn *net.UnixConn, intent StartupIntent, assumption StartupAssumption, publication OwnerPublication) (resultErr error) {
	defer redactOwnerAuthenticationError(&resultErr)
	if err := ownerAuthenticationContext(ctx); err != nil {
		return err
	}
	if _, err := publication.Digest(intent, assumption); err != nil {
		return err
	}
	if err := validateOwnerAuthenticationBoot(ctx, intent); err != nil {
		return err
	}
	peer, err := localpeer.Read(conn)
	if err != nil {
		return err
	}
	if peer.UID != os.Geteuid() || peer.UID != assumption.UID || peer.PID != assumption.Process.PID {
		return ErrIdentity
	}
	// No local-self equality here: a real server may be another native process.
	return requireOwnerProcessPresent(ctx, assumption.Process)
}

func ownerAuthenticationContext(ctx context.Context) error {
	if ctx == nil {
		return os.ErrInvalid
	}
	return ctx.Err()
}

// Must precede any kernel peer-PID read or process-presence observation.
func validateOwnerAuthenticationBoot(ctx context.Context, intent StartupIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return err
	}
	if lifetime.CompareBootIDs(intent.Inputs.BootID, bootID) != lifetime.BootSame {
		return ErrIdentity
	}
	return ctx.Err()
}

func requireOwnerProcessPresent(ctx context.Context, identity ProcessIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reference, err := identity.Reference()
	if err != nil {
		return err
	}
	present, err := lifetime.ProcessPresent(reference)
	if err != nil {
		return err
	}
	if !present {
		return ErrIdentity
	}
	return ctx.Err()
}

func validateOwnerRequestSubject(intent StartupIntent, publicationDigest string, request ownerRequest) error {
	if err := request.validate(); err != nil {
		return err
	}
	if err := request.Owner.validateIntent(intent); err != nil {
		return err
	}
	if request.RootID != intent.Inputs.RootID || request.PublicationSHA256 != publicationDigest {
		return ErrIdentity
	}
	return nil
}

// validateOwnerReplyBinding checks data echoes only, never liveness/readiness.
// The caller must authenticate the socket and retain the current pinned owner.
func validateOwnerReplyBinding(intent StartupIntent, assumption StartupAssumption, publication OwnerPublication, request ownerRequest, reply ownerReply) (resultErr error) {
	defer redactOwnerAuthenticationError(&resultErr)
	digest, err := publication.Digest(intent, assumption)
	if err != nil {
		return err
	}
	if err = validateOwnerRequestSubject(intent, digest, request); err != nil {
		return err
	}
	if err = reply.validate(); err != nil {
		return err
	}
	if !reply.matchesRequestedSubject(request) {
		return ErrIdentity
	}
	if ownPreparationAction(request.Action) {
		expected := originRecord{Root: intent.Inputs.Root, RootID: intent.Inputs.RootID}
		// A refused own operation may omit its target; accepted replies cannot,
		// as required by the wire DTO's canonical own-subject validation.
		if reply.Target != (originRecord{}) && reply.Target != expected {
			return ErrIdentity
		}
	} else if reply.Target != request.Target {
		return ErrIdentity
	}
	return nil
}

// Compare every echoed selection independently from the observed disposition.
func (r ownerReply) matchesRequestedSubject(request ownerRequest) bool {
	return r.RootID == request.RootID && r.OwnerRef == request.Owner && r.PublicationSHA256 == request.PublicationSHA256 && r.Action == request.Action && r.Context == request.Context && r.Source == request.Source && r.Initial == request.Initial && (request.Action == ownerEnsureOther || r.InitialResultFor == request.InitialResultFor)
}

type ownerAuthenticationError struct{ cause error }

func (ownerAuthenticationError) Error() string   { return "infrastructure owner authentication failed" }
func (e ownerAuthenticationError) Unwrap() error { return e.cause }
func redactOwnerAuthenticationError(result *error) {
	if *result != nil {
		*result = ownerAuthenticationError{cause: *result}
	}
}
