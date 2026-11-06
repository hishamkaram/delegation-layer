package infrastructure

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func ownerArgumentFixture() ([]string, ownerInvocation) {
	root := filepath.Join(string(filepath.Separator), "selected-root")
	expected := ownerInvocation{root: root, rootID: strings.Repeat("a", 32), startup: StartupReference{Sequence: 42, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64)}}
	return []string{"--infrastructure-owner", "--root", root, "--root-id", expected.rootID, "--sequence", "42", "--epoch", expected.startup.Epoch, "--intent-sha256", expected.startup.IntentSHA256}, expected
}

func TestOwnerInvocationParsesExplicitVectorAndCanonicalEncoder(t *testing.T) {
	arguments, expected := ownerArgumentFixture()
	before := slices.Clone(arguments)
	got, handled, err := parseOwnerInvocation(arguments)
	if err != nil || !handled || got != expected {
		t.Fatalf("explicit owner vector: handled=%t error=%v", handled, err)
	}
	encoded, err := ownerArguments(expected.root, expected.rootID, expected.startup)
	if err != nil || !slices.Equal(encoded, before) {
		t.Fatalf("noncanonical owner encoding: %v", err)
	}
	encoded[0] = "changed"
	if !slices.Equal(arguments, before) {
		t.Fatal("encoder/parser changed caller argv")
	}
	reordered := []string{arguments[0], arguments[9], arguments[10], arguments[7], arguments[8], arguments[5], arguments[6], arguments[3], arguments[4], arguments[1], arguments[2]}
	got, handled, err = parseOwnerInvocation(reordered)
	if err != nil || !handled || got != expected {
		t.Fatalf("distinct reordered values refused: %v", err)
	}
}

func TestOwnerInvocationAbsentModeIsUnhandled(t *testing.T) {
	for _, arguments := range [][]string{nil, {}, {"--command-owner", "--root", "anything"}, {"normal", "--root=x"}, {"-infrastructure-owner"}, {"--infrastructure-owner-near"}} {
		got, handled, err := parseOwnerInvocation(arguments)
		if err != nil || handled || got != (ownerInvocation{}) {
			t.Fatalf("ordinary invocation handled: %v", err)
		}
	}
}

func TestOwnerInvocationMalformedModeCannotFallThrough(t *testing.T) {
	arguments, _ := ownerArgumentFixture()
	for _, bad := range [][]string{
		{"--infrastructure-owner"},
		{"--infrastructure-owner=value"},
		{"--command-owner", "--infrastructure-owner=value"},
		append([]string{"--command-owner"}, arguments...),
		append(slices.Clone(arguments), "--infrastructure-owner"),
		append(slices.Clone(arguments), "trailing"),
		append([]string{"--infrastructure-owner=value"}, arguments[1:]...),
		append([]string{"--root", arguments[2], "--infrastructure-owner"}, arguments[3:]...),
	} {
		requireMalformedOwnerInvocation(t, bad)
	}
}

func requireMalformedOwnerInvocation(t *testing.T, arguments []string) {
	t.Helper()
	got, handled, err := parseOwnerInvocation(arguments)
	if !handled || !errors.Is(err, ErrIdentity) || got != (ownerInvocation{}) {
		t.Fatalf("malformed reserved invocation fell through/accepted: handled=%t error=%v", handled, err)
	}
}

func TestOwnerInvocationRejectsFlagAndValueVariants(t *testing.T) {
	arguments, _ := ownerArgumentFixture()
	for _, change := range []struct {
		index int
		value string
	}{
		{1, "--unknown"},
		{1, "-root"},
		{1, "--root=" + arguments[2]},
		{3, "--root"},
		{5, "--infrastructure-owner"},
		{2, ""},
		{2, "relative"},
		{2, arguments[2] + "/../dirty"},
		{2, arguments[2] + "\x00"},
		{4, "bad"},
		{4, strings.Repeat("A", 32)},
		{6, "0"},
		{6, "01"},
		{6, "+1"},
		{6, "-1"},
		{6, "1.0"},
		{6, " 1"},
		{6, "18446744073709551616"},
		{8, "bad"},
		{8, strings.Repeat("B", 32)},
		{10, "bad"},
		{10, strings.Repeat("C", 64)},
	} {
		changed := slices.Clone(arguments)
		changed[change.index] = change.value
		requireMalformedOwnerInvocation(t, changed)
	}
	for index := 1; index < len(arguments); index++ {
		changed := append(slices.Clone(arguments[:index]), arguments[index+1:]...)
		requireMalformedOwnerInvocation(t, changed)
	}
}

func TestOwnerArgumentsRejectInvalidData(t *testing.T) {
	_, valid := ownerArgumentFixture()
	for _, kind := range []string{"root", "root-id", "sequence", "epoch", "digest"} {
		changed := valid
		switch kind {
		case "root":
			changed.root = "relative"
		case "root-id":
			changed.rootID = "bad"
		case "sequence":
			changed.startup.Sequence = 0
		case "epoch":
			changed.startup.Epoch = "bad"
		case "digest":
			changed.startup.IntentSHA256 = "bad"
		}
		arguments, err := ownerArguments(changed.root, changed.rootID, changed.startup)
		if !errors.Is(err, ErrIdentity) || arguments != nil {
			t.Fatalf("invalid %s encoded: %v", kind, err)
		}
	}
}

func TestOwnerInvocationAcceptsMaximumCanonicalSequence(t *testing.T) {
	arguments, expected := ownerArgumentFixture()
	arguments[6] = "18446744073709551615"
	expected.startup.Sequence = ^uint64(0)
	got, handled, err := parseOwnerInvocation(arguments)
	if err != nil || !handled || got != expected {
		t.Fatalf("maximum canonical sequence refused: %v", err)
	}
	encoded, err := ownerArguments(expected.root, expected.rootID, expected.startup)
	if err != nil || !slices.Equal(encoded, arguments) {
		t.Fatalf("maximum canonical sequence changed: %v", err)
	}
}
