package pi

import (
	"reflect"
	"strings"
	"testing"
)

func TestModelDiscoveryProjectsObservedTable(t *testing.T) {
	definition := ModelDiscovery()
	if !reflect.DeepEqual(definition.Arguments, []string{"--list-models"}) || definition.Source != modelDiscoverySource || definition.NewExchange != nil {
		t.Fatalf("unexpected discovery definition: %+v", definition)
	}
	help := []byte("  --thinking <level>             Set thinking level: off, minimal, low, medium, high, xhigh, max\n")
	stdout := []byte("provider    model                                               context  max-out  thinking  images\n" +
		"openrouter  anthropic/claude-sonnet-4.6                         1M       64K     yes       yes\n" +
		"openrouter  openai/gpt-5                                       400K     128K    yes       no\n")
	catalog, err := definition.Project(stdout, nil, help, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || !catalog.Complete || len(catalog.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if got := catalog.Models[0].ID; got != "openrouter/anthropic/claude-sonnet-4.6" {
		t.Fatalf("unexpected exact Pi model selector %q", got)
	}
	if catalog.Models[0].Efforts != nil {
		t.Fatalf("yes/no capability column became effort metadata: %#v", catalog.Models[0].Efforts)
	}
	if !reflect.DeepEqual(catalog.Efforts, []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("unexpected thinking levels: %#v", catalog.Efforts)
	}
}

func TestModelDiscoveryRejectsUnknownTableShape(t *testing.T) {
	_, _, err := parseModelTable([]byte("provider model\nopenai gpt-5\n"))
	if err == nil {
		t.Fatal("unknown Pi table shape was accepted")
	}
}

func TestModelDiscoveryClassifiesPermissionAndAuth(t *testing.T) {
	failed, err := projectModels(nil, []byte("permission denied"), nil, false)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("permission failure was not failed: %+v %v", failed, err)
	}
	blocked, err := projectModels(nil, []byte("login required"), nil, false)
	if err != nil || blocked.Status != "blocked" || len(blocked.Models) != 0 {
		t.Fatalf("auth failure was not blocked: %+v %v", blocked, err)
	}
}

func nativeEmptyModelListing(docsDirectory, lineEnding string) string {
	// Pi 1.1.0's public auth-guidance formatter writes these three lines.
	return strings.Join([]string{
		"No models available. Use /login to log into a provider via OAuth or API key. See:",
		"  " + docsDirectory + "/providers.md",
		"  " + docsDirectory + "/models.md",
		"",
	}, lineEnding)
}

func TestModelDiscoveryProjectsNativeEmptyListing(t *testing.T) {
	for _, test := range []struct {
		name          string
		docsDirectory string
		lineEnding    string
	}{
		{name: "linux", docsDirectory: "/opt/pi-coding-agent/docs", lineEnding: "\n"},
		{name: "package-path-with-spaces", docsDirectory: "/Applications/Pi Agent/docs", lineEnding: "\n"},
		{name: "crlf", docsDirectory: "/Users/example/Pi Agent/docs", lineEnding: "\r\n"},
		{name: "path-is-not-an-auth-diagnostic", docsDirectory: "/opt/no API key/pi/docs", lineEnding: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, err := ModelDiscovery().Project([]byte(nativeEmptyModelListing(test.docsDirectory, test.lineEnding)), nil, nil, true)
			if err != nil {
				t.Fatalf("native empty listing was rejected: %v", err)
			}
			if catalog.Status != "unavailable" || catalog.ReasonCode != "models_unavailable" || catalog.Complete {
				t.Fatalf("native empty listing was not unavailable: %+v", catalog)
			}
			if catalog.Models == nil || len(catalog.Models) != 0 || catalog.Efforts != nil || catalog.Source != modelDiscoverySource {
				t.Fatalf("native empty listing invented or lost catalog facts: %+v", catalog)
			}
		})
	}
}

func TestModelDiscoveryRejectsMalformedNativeEmptyListing(t *testing.T) {
	listing := nativeEmptyModelListing("/opt/pi/docs", "\n")
	for _, test := range []struct {
		name   string
		stdout string
	}{
		{name: "message-only", stdout: strings.SplitN(listing, "\n", 2)[0]},
		{name: "different-prefix", stdout: strings.Replace(listing, "No models available.", "No models found.", 1)},
		{name: "leading-output", stdout: "Warning: model scan\n" + listing},
		{name: "trailing-output", stdout: listing + "unexpected output\n"},
		{name: "trailing-blank-line", stdout: listing + "\n"},
		{name: "mixed-table-output", stdout: listing + "provider model context max-out thinking images\n"},
		{name: "missing-models-reference", stdout: strings.Replace(listing, "  /opt/pi/docs/models.md\n", "", 1)},
		{name: "different-parents", stdout: strings.Replace(listing, "/opt/pi/docs/models.md", "/opt/other/docs/models.md", 1)},
		{name: "wrong-reference", stdout: strings.Replace(listing, "providers.md", "provider.md", 1)},
		{name: "wrong-directory", stdout: nativeEmptyModelListing("/opt/pi/manual", "\n")},
		{name: "relative-path", stdout: nativeEmptyModelListing("pi/docs", "\n")},
		{name: "unclean-path", stdout: nativeEmptyModelListing("/opt/pi/../other/docs", "\n")},
		{name: "control-in-path", stdout: nativeEmptyModelListing("/opt/pi\t/docs", "\n")},
		{name: "invalid-utf8-path", stdout: nativeEmptyModelListing("/opt/pi\xff/docs", "\n")},
		{name: "wrong-indentation", stdout: strings.Replace(listing, "  /opt/pi/docs/providers.md", " /opt/pi/docs/providers.md", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if catalog, err := ModelDiscovery().Project([]byte(test.stdout), nil, nil, true); err == nil {
				t.Fatalf("malformed empty listing became a catalog: %+v", catalog)
			}
		})
	}
}

func TestModelDiscoveryNativeEmptyListingRequiresSuccessfulExit(t *testing.T) {
	stdout := []byte(nativeEmptyModelListing("/opt/pi/docs", "\n"))
	catalog, err := ModelDiscovery().Project(stdout, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "failed" || catalog.ReasonCode != "native_command_failed" || catalog.Complete || len(catalog.Models) != 0 {
		t.Fatalf("unsuccessful native empty listing was not failed: %+v", catalog)
	}
}

func TestModelDiscoveryPreservesStrictTableParsing(t *testing.T) {
	const header = "provider model context max-out thinking images\n"
	for _, stdout := range []string{
		header + "openai gpt-5 400K 128K yes\n",
		header + "openai gpt-5 400K 128K yes no extra\n",
		header + "openai gpt-5 400K 128K yes no\nopenai gpt-5 400K 128K yes no\n",
	} {
		if catalog, err := ModelDiscovery().Project([]byte(stdout), nil, nil, true); err == nil {
			t.Fatalf("malformed model table became a catalog: %+v", catalog)
		}
	}
	catalog, err := ModelDiscovery().Project([]byte(header), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "unavailable" || catalog.ReasonCode != "models_unavailable" || catalog.Complete || catalog.Models == nil || len(catalog.Models) != 0 {
		t.Fatalf("empty table changed classification: %+v", catalog)
	}
}
