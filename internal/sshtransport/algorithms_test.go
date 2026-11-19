package sshtransport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"golang.org/x/crypto/ssh"
)

const curve25519LibSSH = "curve25519-sha256@libssh.org"

func configuredAlgorithms(t *testing.T, config string) sshconfig.AlgorithmPolicy {
	t.Helper()
	home := t.TempDir()
	file := filepath.Join(home, "ssh_config")
	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := sshconfig.Load(sshconfig.Options{Home: home, LocalUser: "fixture", ConfigFile: file})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := loaded.Resolve("fixture")
	if err != nil {
		t.Fatal(err)
	}
	return connection.Algorithms
}

func TestConfiguredAlgorithmOrderOnWire(t *testing.T) {
	policy := configuredAlgorithms(t, "Ciphers ^aes256-ctr\nKexAlgorithms ^ecdh-sha2-nistp256\nMACs ^hmac-sha2-256\n")
	host := key(t)
	observations := make(chan ssh.NegotiatedAlgorithms, 2)
	connection, dir := configuredServer(t, host, nil, func(config *ssh.ServerConfig) {
		// Each peer list starts with the alternative. The selected cipher is
		// non-AEAD so a matching MAC must actually be negotiated.
		config.Ciphers = []string{ssh.CipherAES128CTR, ssh.CipherAES256CTR}
		config.KeyExchanges = []string{ssh.KeyExchangeCurve25519, ssh.KeyExchangeECDHP256}
		config.MACs = []string{ssh.HMACSHA256ETM, ssh.HMACSHA256}
		config.NoClientAuth = true
		config.NoClientAuthCallback = func(metadata ssh.ConnMetadata) (*ssh.Permissions, error) {
			source, ok := metadata.(ssh.AlgorithmsConnMetadata)
			if !ok {
				return nil, errors.New("loopback peer did not receive algorithm metadata")
			}
			select {
			case observations <- source.Algorithms():
				return nil, nil
			default:
				return nil, errors.New("unexpected extra SSH connection")
			}
		}
	})
	connection.Algorithms = policy
	options := Options{KnownHostsFile: filepath.Join(dir, "known_hosts"), WithTrustLock: trustLock(t)}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := Enroll(ctx, connection, options, ssh.FingerprintSHA256(host.PublicKey())); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	requireWireAlgorithms(t, ctx, observations, "enroll")
	client, err := Dial(ctx, connection, options)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	requireWireAlgorithms(t, ctx, observations, "dial")
}

func requireWireAlgorithms(t *testing.T, ctx context.Context, observations <-chan ssh.NegotiatedAlgorithms, operation string) {
	t.Helper()
	var got ssh.NegotiatedAlgorithms
	select {
	case got = <-observations:
	case <-ctx.Done():
		t.Fatalf("%s did not negotiate algorithms: %v", operation, ctx.Err())
	}
	if got.KeyExchange != ssh.KeyExchangeECDHP256 {
		t.Errorf("%s key exchange = %q, want %q", operation, got.KeyExchange, ssh.KeyExchangeECDHP256)
	}
	for _, direction := range []ssh.DirectionAlgorithms{got.Read, got.Write} {
		if direction.Cipher != ssh.CipherAES256CTR || direction.MAC != ssh.HMACSHA256 {
			t.Errorf("%s algorithms = %#v, want cipher %q and MAC %q", operation, direction, ssh.CipherAES256CTR, ssh.HMACSHA256)
		}
	}
}

func TestDisjointAlgorithmPolicyDoesNotEnroll(t *testing.T) {
	host := key(t)
	connection, dir := configuredServer(t, host, nil, func(config *ssh.ServerConfig) {
		config.Ciphers = []string{ssh.CipherAES128CTR}
	})
	connection.Algorithms = configuredAlgorithms(t, "Ciphers -aes128-ctr\n")
	trust := filepath.Join(dir, "known_hosts")
	options := Options{KnownHostsFile: trust, WithTrustLock: trustLock(t)}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	requireNegotiationFailure(t, Enroll(ctx, connection, options, ssh.FingerprintSHA256(host.PublicKey())))
	if _, err := os.Stat(trust); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed enrollment wrote trust: %v", err)
	}
	// Permit the known host independently so Dial reaches SSH negotiation.
	connection.Algorithms = sshconfig.AlgorithmPolicy{}
	if err := Enroll(ctx, connection, options, ssh.FingerprintSHA256(host.PublicKey())); err != nil {
		t.Fatal(err)
	}
	connection.Algorithms = configuredAlgorithms(t, "Ciphers aes256-ctr\n")
	client, err := Dial(ctx, connection, options)
	if client != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	requireNegotiationFailure(t, err)
}

func requireNegotiationFailure(t *testing.T, err error) {
	t.Helper()
	var failure *Error
	var negotiation *ssh.AlgorithmNegotiationError
	if !errors.As(err, &failure) || failure.Code != "ssh_algorithm_negotiation_failed" || !errors.As(err, &negotiation) {
		t.Fatalf("SSH algorithm mismatch was not classified: %v", err)
	}
}

func TestUnrelatedSSHNegotiationRetainsHandshakeFailure(t *testing.T) {
	for _, category := range []string{"host key", "server to client compression"} {
		t.Run(category, func(t *testing.T) {
			cause := &ssh.AlgorithmNegotiationError{What: category}
			var failure *Error
			err := classifyClientHandshakeError(sshconfig.Connection{}, cause)
			if !errors.As(err, &failure) || failure.Code != "ssh_handshake_failed" || !errors.Is(err, cause) {
				t.Fatalf("unrelated SSH negotiation was misclassified: %v", err)
			}
		})
	}
}

func TestAlgorithmPolicyRefusedBeforeSSHSideEffects(t *testing.T) {
	for _, test := range []struct{ name, input, code, directive string }{
		{"partial positive list", "Ciphers aes128-ctr,invalid-cipher\n", "unsupported_ssh_algorithm_policy", "ciphers"},
		{"explicit legacy default", "MACs +hmac-sha1-96\n", "unsupported_ssh_algorithm_policy", "macs"},
		{"empty effective list", "Ciphers -*\n", "invalid_ssh_setting", "ciphers"},
		{"unrepresentable exclusion", "KexAlgorithms -curve25519-sha256@libssh.org\n", "ssh_algorithm_policy_unrepresentable", "kexalgorithms"},
		{"unrepresentable replacement", "KexAlgorithms curve25519-sha256\n", "ssh_algorithm_policy_unrepresentable", "kexalgorithms"},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := sshconfig.Connection{
				HostName:      "127.0.0.1",
				User:          "fixture",
				IdentityAgent: "none",
				IdentityFiles: []string{filepath.Join(t.TempDir(), "missing-identity")},
				Algorithms:    configuredAlgorithms(t, test.input),
			}
			options := Options{KnownHostsFile: filepath.Join(t.TempDir(), "missing-parent", "known_hosts"), WithTrustLock: trustLock(t)}
			requireAlgorithmConfigError(t, Enroll(t.Context(), connection, options, "SHA256:unused"), test.code, test.directive)
			_, err := Dial(t.Context(), connection, options)
			requireAlgorithmConfigError(t, err, test.code, test.directive)
		})
	}
}

func requireAlgorithmConfigError(t *testing.T, err error, code, directive string) {
	t.Helper()
	var configError *sshconfig.Error
	if !errors.As(err, &configError) || configError.Code != code || filepath.Base(configError.File) != "ssh_config" || configError.Line != 1 || configError.Directive != directive {
		t.Fatalf("want source-located %s error for %s, got %v", code, directive, err)
	}
}

func TestCompilePolicyPreservesDefaultsAndSelection(t *testing.T) {
	var defaults ssh.Config
	defaults.SetDefaults()
	policy := configuredAlgorithms(t, "Ciphers -nomatch?,aes128-ctr\nKexAlgorithms +diffie-hellman-group16-sha512,diffie-hellman-group16-sha512\nMACs -hmac-sha1-96\n")
	got, err := compileAlgorithms(policy)
	if err != nil {
		t.Fatal(err)
	}
	wantCiphers := withoutAlgorithm(defaults.Ciphers, ssh.CipherAES128CTR)
	wantKex := append(slices.Clone(defaults.KeyExchanges), ssh.KeyExchangeDH16SHA512)
	wantMACs := withoutAlgorithm(defaults.MACs, ssh.InsecureHMACSHA196)
	if !slices.Equal(got.Ciphers, wantCiphers) || !slices.Equal(got.KeyExchanges, wantKex) || !slices.Equal(got.MACs, wantMACs) {
		t.Fatalf("compiled order differs from requested defaults-based modifiers: %#v", got)
	}
	policy = configuredAlgorithms(t, "KexAlgorithms -diffie-hellman-group14-sha1,curve25519*\n")
	got, err = compileAlgorithms(policy)
	if err != nil {
		t.Fatal(err)
	}
	wantKex = withoutAlgorithm(defaults.KeyExchanges, ssh.InsecureKeyExchangeDH14SHA1, ssh.KeyExchangeCurve25519, curve25519LibSSH)
	if got.Ciphers != nil || got.MACs != nil || !slices.Equal(got.KeyExchanges, wantKex) {
		t.Fatalf("omitted categories changed or removal lost wire order: %#v", got)
	}
}

func TestCompilePolicyRejectsUnknownModifier(t *testing.T) {
	policy := configuredAlgorithms(t, "Ciphers aes128-ctr\n")
	policy.Ciphers.Modifier = '!'
	_, err := compileAlgorithms(policy)
	requireAlgorithmConfigError(t, err, "invalid_ssh_setting", "ciphers")
}

func TestCompilePolicyAllowsExplicitCurve25519WireNames(t *testing.T) {
	policy := configuredAlgorithms(t, "KexAlgorithms curve25519-sha256,curve25519-sha256@libssh.org\n")
	got, err := compileAlgorithms(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.KeyExchanges, []string{ssh.KeyExchangeCurve25519, curve25519LibSSH}) {
		t.Fatalf("explicit Curve25519 wire list changed: %q", got.KeyExchanges)
	}
}

func withoutAlgorithm(algorithms []string, removed ...string) []string {
	var result []string
	for _, name := range algorithms {
		if !slices.Contains(removed, name) {
			result = append(result, name)
		}
	}
	return result
}
