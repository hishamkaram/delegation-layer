package app

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func TestTeamsCatalogPathUsesCapturedEnvironmentAndInjection(t *testing.T) {
	dir := t.TempDir()
	deps := Dependencies{Environment: []string{"HOME=" + dir, "XDG_CONFIG_HOME=" + filepath.Join(dir, "captured"), "AppData=" + filepath.Join(dir, "captured")}}
	path, err := resolveTeamsCatalogPath(deps)
	configDir := filepath.Join(dir, "captured")
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		configDir = filepath.Join(dir, "Library", "Application Support")
	}
	want, canonicalErr := config.CanonicalizePath(filepath.Join(configDir, "delegation-layer-catalog", "teams.sqlite"))
	if err != nil || canonicalErr != nil || path != want {
		t.Fatalf("captured catalog path=%s want=%s err=%v/%v", path, want, err, canonicalErr)
	}
	deps.TeamCatalogPath = filepath.Join(dir, "injected.sqlite")
	injected, err := resolveTeamsCatalogPath(deps)
	want, canonicalErr = config.CanonicalizePath(deps.TeamCatalogPath)
	if err != nil || canonicalErr != nil || injected != want {
		t.Fatalf("injected catalog path=%s want=%s err=%v/%v", injected, want, err, canonicalErr)
	}
	for _, invalid := range []Dependencies{{}, {Environment: []string{}}, {TeamCatalogPath: "relative"}} {
		if _, err = resolveTeamsCatalogPath(invalid); err == nil {
			t.Fatalf("accepted missing/relative configuration: %+v", invalid)
		}
	}
}

func TestTeamsDefaultsDoNotRequireTaskOptionsOrDiscoverModels(t *testing.T) {
	catalog := NativeCatalog()
	for _, defaults := range []contract.ExecutionDefaults{
		{Provider: "ccr:print"},
		{Provider: "codex:exec", Model: "literal-unlisted-provider-alias", Effort: "max"},
	} {
		if err := validateTeamsDefaults(catalog, defaults); err != nil {
			t.Fatalf("valid supplied defaults rejected: %+v: %v", defaults, err)
		}
	}
	for _, defaults := range []contract.ExecutionDefaults{
		{Provider: "unknown:print"}, {Provider: "fixture"},
	} {
		if err := validateTeamsDefaults(catalog, defaults); !errors.Is(err, contract.ErrInvalidDefinition) {
			t.Fatalf("unsupported defaults did not fail explicitly: %+v: %v", defaults, err)
		}
	}
}

func TestTeamsDefaultsRejectOnlySuppliedUnsupportedCapabilities(t *testing.T) {
	registration, err := NativeCatalog().Lookup("codex:exec")
	if err != nil {
		t.Fatal(err)
	}
	registration.Description.SupportedOptions = []string{commonprovider.OptionInteractive, commonprovider.OptionContinuation}
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	for _, defaults := range []contract.ExecutionDefaults{
		{Provider: "codex:exec"}, {Provider: "codex:exec", Effort: "default"},
	} {
		if err = validateTeamsDefaults(catalog, defaults); err != nil {
			t.Fatalf("omitted/provider-default option rejected: %v", err)
		}
	}
	for _, defaults := range []contract.ExecutionDefaults{
		{Provider: "codex:exec", Model: "literal"}, {Provider: "codex:exec", Effort: "high"},
	} {
		if err = validateTeamsDefaults(catalog, defaults); !errors.Is(err, contract.ErrInvalidDefinition) {
			t.Fatalf("unsupported supplied option accepted: %+v: %v", defaults, err)
		}
	}
}
