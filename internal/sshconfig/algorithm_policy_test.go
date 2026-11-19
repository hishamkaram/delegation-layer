package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAppleSystemCryptoIncludeResolves(t *testing.T) {
	crypto := filepath.Join(t.TempDir(), "crypto.conf")
	data := "Ciphers ^aes128-gcm@openssh.com,aes256-gcm@openssh.com\n" +
		"KexAlgorithms ^ecdh-sha2-nistp256\n" +
		"MACs ^hmac-sha2-256-etm@openssh.com,hmac-sha2-256\n"
	if err := os.WriteFile(crypto, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	config := configuration(t, "Host worker\n HostName worker.example\n",
		"Host *\n SendEnv LANG LC_*\n Include \""+crypto+"\"\n", nil)
	got, err := config.Resolve("worker")
	if err != nil {
		t.Fatalf("resolve Apple system crypto include: %v", err)
	}
	want := AlgorithmPolicy{
		Ciphers: AlgorithmList{
			Modifier: '^', Names: []string{"aes128-gcm@openssh.com", "aes256-gcm@openssh.com"},
			File: crypto, Line: 1, Directive: "ciphers",
		},
		KeyExchanges: AlgorithmList{
			Modifier: '^', Names: []string{"ecdh-sha2-nistp256"},
			File: crypto, Line: 2, Directive: "kexalgorithms",
		},
		MACs: AlgorithmList{
			Modifier: '^', Names: []string{"hmac-sha2-256-etm@openssh.com", "hmac-sha2-256"},
			File: crypto, Line: 3, Directive: "macs",
		},
	}
	if !reflect.DeepEqual(got.Algorithms, want) {
		t.Fatalf("resolved policy: %+v, want %+v", got.Algorithms, want)
	}
}

func TestAlgorithmPolicyUsesFirstApplicableDirective(t *testing.T) {
	user := "Host other\n Ciphers ^!unselected\n" +
		"Host worker\n Ciphers aes256-ctr\n MACs -hmac-sha1-?,umac-*\n" +
		"Host *\n Ciphers ^!shadowed\n"
	system := "Ciphers ^aes128-ctr\n KexAlgorithms +ecdh-sha2-nistp521\n"
	config := configuration(t, user, system, nil)
	got, err := config.Resolve("worker")
	if err != nil {
		t.Fatal(err)
	}
	want := AlgorithmPolicy{
		Ciphers: AlgorithmList{
			Names: []string{"aes256-ctr"}, File: config.options.UserConfig, Line: 4, Directive: "ciphers",
		},
		KeyExchanges: AlgorithmList{
			Modifier: '+', Names: []string{"ecdh-sha2-nistp521"},
			File: config.options.SystemConfig, Line: 2, Directive: "kexalgorithms",
		},
		MACs: AlgorithmList{
			Modifier: '-', Names: []string{"hmac-sha1-?", "umac-*"},
			File: config.options.UserConfig, Line: 5, Directive: "macs",
		},
	}
	if !reflect.DeepEqual(got.Algorithms, want) {
		t.Fatalf("resolved policy: %+v, want %+v", got.Algorithms, want)
	}
}

func TestMalformedAlgorithmListReportsSelectedSource(t *testing.T) {
	for _, setting := range []string{
		`""`, "^", "aes128-ctr,",
		"^aes*", "+aes128-ctr,aes?", "-!aes*",
		"-aes[12]*", "-aes/ctr", "aes128-ctr aes256-ctr",
	} {
		t.Run(setting, func(t *testing.T) {
			config := configuration(t, "", "Host worker\n Ciphers "+setting+"\n", nil)
			_, err := config.Resolve("worker")
			var policyErr *Error
			if !errors.As(err, &policyErr) ||
				policyErr.Code != "invalid_ssh_setting" || policyErr.Directive != "ciphers" ||
				policyErr.File != config.options.SystemConfig || policyErr.Line != 2 {
				t.Fatalf("expected source-located invalid Ciphers setting, got %v", err)
			}
		})
	}
}
