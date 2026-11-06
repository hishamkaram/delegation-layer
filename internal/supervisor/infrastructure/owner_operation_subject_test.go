package infrastructure

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

func ownerPreparationSubjectFixture(action ownerAction) ownerRequest {
	r := ownerRequestFixture()
	r.Action = action
	r.Context = LaunchContextReference{SHA256: ownerWireDigest}
	switch action {
	case ownerRecoverSaved, ownerPrepareContinuation:
		r.Source = SavedBindingSource{RootID: r.RootID, Kind: SavedBindingTaskMetadata, OwnerID: strings.Repeat("b", 32), BindingSHA256: strings.Repeat("c", 64)}
	case ownerEnsureOwn, ownerMigrateOwn:
	case ownerEnsureOther:
		r.Target = originRecord{Root: "/other", RootID: strings.Repeat("b", 32)}

	case ownerStatus:
		r.Context = LaunchContextReference{}
	}
	return r
}

func TestOwnerPreparationSubjectsRoundTripAndRefuseMixedAuthority(t *testing.T) {
	for _, action := range []ownerAction{ownerEnsureOwn, ownerEnsureOther, ownerRecoverSaved, ownerPrepareContinuation} {
		t.Run(string(action), func(t *testing.T) {
			request := ownerPreparationSubjectFixture(action)
			var wire bytes.Buffer
			if err := writeOwnerFrame(&wire, request); err != nil {
				t.Fatal(err)
			}
			got, err := readOwnerFrame[ownerRequest](bufio.NewReader(&wire))
			if err != nil || got != request {
				t.Fatalf("selected subject changed: %v", err)
			}
			for _, kind := range []string{"source", "context", "target"} {
				changed := request
				switch kind {
				case "source":
					changed.Source = ownerPreparationSubjectFixture(ownerRecoverSaved).Source
					changed.Source.RootID = strings.Repeat("e", 32)
				case "context":
					changed.Context.SHA256 = "invalid"
				case "target":
					changed.Target = originRecord{Root: "relative", RootID: strings.Repeat("b", 32)}
				}
				if err := writeOwnerFrame(&wire, changed); !errors.Is(err, ErrIdentity) {
					t.Fatalf("%s accepted mixed/invalid %s: %v", action, kind, err)
				}
			}
		})
	}
}

func TestOwnerReplyBindsSelectedContextAndExactCanonicalLocators(t *testing.T) {
	fixture := newOwnerAuthFixture(t)
	for _, action := range []ownerAction{ownerEnsureOwn, ownerRecoverSaved, ownerPrepareContinuation} {
		request := ownerPreparationSubjectFixture(action)
		request.Owner, request.RootID, request.PublicationSHA256, request.Client = fixture.request.Owner, fixture.request.RootID, fixture.request.PublicationSHA256, fixture.request.Client
		if action == ownerRecoverSaved || action == ownerPrepareContinuation {
			request.Source.RootID = request.RootID
		}
		reply := ownerReply{Protocol: request.Protocol, RootID: request.RootID, OwnerRef: request.Owner, PublicationSHA256: request.PublicationSHA256, Action: action, Target: originRecord{Root: fixture.root, RootID: request.RootID}, Startup: request.Owner, Code: ownerOK, Context: request.Context, Source: request.Source, Disposition: Disposition{Kind: DispositionStarted, Context: request.Context}}
		if err := validateOwnerReplyBinding(fixture.intent, fixture.assumption, fixture.publication, request, reply); err != nil {
			t.Fatal(err)
		}
		assertOwnerReplyLocatorBinding(t, fixture, request, reply)
		reply.Code = ownerFault
		if err := reply.validate(); err != nil {
			t.Fatalf("late failure lost started disposition: %v", err)
		}
		reply.Code, reply.Disposition = ownerOK, Disposition{}
		if err := reply.validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("success without preparation observation accepted: %v", err)
		}
	}
}

func assertOwnerReplyLocatorBinding(t *testing.T, fixture *ownerAuthFixture, request ownerRequest, reply ownerReply) {
	t.Helper()
	for _, kind := range []string{"context", "source"} {
		changed := reply
		switch kind {
		case "context":
			changed.Context.SHA256 = strings.Repeat("d", 64)
		case "source":
			if request.Action != ownerRecoverSaved && request.Action != ownerPrepareContinuation {
				continue
			}
			changed.Source.BindingSHA256 = strings.Repeat("d", 64)
		}
		// The altered reply is independently valid. Refusal must come from
		// binding it to the original request, not malformed-value rejection.
		if err := changed.validate(); err != nil {
			t.Fatal(err)
		}
		if err := validateOwnerReplyBinding(fixture.intent, fixture.assumption, fixture.publication, request, changed); !errors.Is(err, ErrIdentity) {
			t.Fatalf("%s changed %s accepted: %v", request.Action, kind, err)
		}
	}
}
