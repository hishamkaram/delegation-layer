package control

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func installedIdentityLegacyRef(t *testing.T) (string, task.SupervisorRef) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	return root, task.SupervisorRef{
		ClientExecutable:     filepath.Join(root, ".supervisor", "pueue-pair-"+strings.Repeat("a", 64), "pueue"),
		ClientSHA256:         strings.Repeat("b", 64),
		DaemonExecutable:     filepath.Join(root, ".supervisor", "pueue-pair-"+strings.Repeat("a", 64), "pueued"),
		DaemonSHA256:         strings.Repeat("c", 64),
		ResolvedConfigSHA256: strings.Repeat("d", 64),
		Endpoint:             "unix:" + filepath.Join(root, "run", "p.sock"),
		ConfigPath:           filepath.Join(root, ".supervisor", "pueue.yml"),
		ConfigDigest:         strings.Repeat("e", 64), ObservedVersion: "pueue 4.0.4",
	}
}

func TestNewClientRequiresInstalledSelectionForHistoricalCopy(t *testing.T) {
	root, legacy := installedIdentityLegacyRef(t)
	if !IsLegacyStateSupervisorClient(root, legacy) {
		t.Fatal("complete historical reserved layout was not recognized")
	}
	if client, err := NewClient(legacy, Options{}); !errors.Is(err, ErrBinding) || client != nil {
		t.Fatalf("historical copy gained implicit execution: client=%v err=%v", client, err)
	}
	selected := filepath.Join(filepath.Dir(root), "installed", "pueue")
	client, err := NewClient(legacy, Options{RuntimeClientExecutable: selected})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if client.Binding() != legacy {
		t.Fatal("installed transport rewrote durable legacy authority")
	}
	actual := client.RuntimeBinding()
	if actual.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || actual.ClientExecutable != selected || actual.ClientSHA256 != "" || actual.DaemonSHA256 != "" || !task.SameSupervisorIdentity(actual, legacy) {
		t.Fatalf("installed transport lost representation or logical identity: %+v", actual)
	}
}

func TestHistoricalCopyClassificationUsesExactReservedLayout(t *testing.T) {
	root, original := installedIdentityLegacyRef(t)
	cases := []struct {
		name  string
		value task.SupervisorRef
	}{
		{"ordinary installation", original},
		{"different root config", original},
		{"bad directory identity", original},
		{"nested directory", original},
		{"different member", original},
		{"marked generation", original},
	}
	cases[0].value.ClientExecutable = filepath.Join(filepath.Dir(root), "installed", "pueue")
	cases[1].value.ConfigPath = filepath.Join(filepath.Dir(root), "other", ".supervisor", "pueue.yml")
	cases[2].value.ClientExecutable = filepath.Join(root, ".supervisor", "pueue-pair-invalid", "pueue")
	cases[3].value.ClientExecutable = filepath.Join(root, ".supervisor", "extra", "pueue-pair-"+strings.Repeat("a", 64), "pueue")
	cases[4].value.ClientExecutable = filepath.Join(filepath.Dir(original.ClientExecutable), "unrelated")
	cases[5].value.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if IsLegacyStateSupervisorClient(root, test.value) {
				t.Fatal("ordinary or malformed selection classified as historical copy")
			}
		})
	}
}

func TestObservationReceiptPreservesInstalledExecutableIdentity(t *testing.T) {
	_, binding := installedIdentityLegacyRef(t)
	binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	binding.ClientSHA256, binding.DaemonSHA256 = "", ""
	observed := Observation{Matched: true, NumericTaskID: 7, Binding: binding, Identity: Identity{
		RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64),
	}}
	receipt, err := observed.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SupervisorRef() != binding {
		t.Fatal("receipt dropped installed executable representation")
	}
	if err = task.ValidateSupervisorReceipt(receipt); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientRejectsHistoricalCopyAsInstalledSelection(t *testing.T) {
	_, historical := installedIdentityLegacyRef(t)
	client, err := NewClient(historical, Options{RuntimeClientExecutable: historical.ClientExecutable})
	if client != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, ErrBinding) || client != nil {
		t.Fatalf("historical executable copy accepted as installed transport: client=%t err=%v", client != nil, err)
	}
}

func TestNewClientRejectsHistoricalCopyWithFreshMarker(t *testing.T) {
	_, binding := installedIdentityLegacyRef(t)
	binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	binding.ClientSHA256, binding.DaemonSHA256 = "", ""
	client, err := NewClient(binding, Options{})
	if client != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, ErrBinding) || client != nil {
		t.Fatalf("fresh marker authorized a historical executable copy: client=%t err=%v", client != nil, err)
	}
}
