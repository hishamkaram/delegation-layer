package gatewaycommand

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// Only the enrollment wire is simulated. Connect, credential persistence,
// profile migration, source CAS and subsequent public SDK reads are real.
type upgradeEnrollment struct {
	server   *httptest.Server
	ca       *x509.Certificate
	key      *ecdsa.PrivateKey
	requests atomic.Int32
}

type upgradeDescriptor struct {
	SchemaVersion         int      `json:"schema_version"`
	GatewayID             string   `json:"gateway_id"`
	PublicURL             string   `json:"public_url"`
	Issuer                string   `json:"issuer"`
	NativeClientID        string   `json:"native_client_id"`
	Audience              string   `json:"audience"`
	EnrollmentURL         string   `json:"enrollment_url"`
	SelfEnrollmentURL     string   `json:"self_enrollment_url"`
	RevokeURL             string   `json:"revoke_url"`
	DeviceWSSURL          string   `json:"device_wss_url"`
	DeviceCAPEM           string   `json:"device_ca_pem"`
	SupportedWireVersions []int    `json:"supported_wire_versions"`
	Capabilities          []string `json:"capabilities"`
}

func newUpgradeEnrollment(t *testing.T) *upgradeEnrollment {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "upgrade fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &upgradeEnrollment{ca: ca, key: key}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *upgradeEnrollment) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	base := f.server.URL
	switch r.URL.Path {
	case "/.well-known/delegation-gateway":
		descriptor := upgradeDescriptor{SchemaVersion: 1, GatewayID: "upgrade-gateway", PublicURL: base, Issuer: base + "/", NativeClientID: "native-client", Audience: base + "/mcp", EnrollmentURL: base + "/v1/enroll", SelfEnrollmentURL: base + "/v1/self/enroll", RevokeURL: base + "/v1/self/devices/revoke", DeviceWSSURL: "wss://" + strings.TrimPrefix(base, "https://") + "/device", DeviceCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw})), SupportedWireVersions: []int{1, 2}, Capabilities: []string{"self-enrollment-v1"}}
		if err := json.NewEncoder(w).Encode(descriptor); err != nil {
			http.Error(w, "fixture encoding failed", 500)
		}
	case "/.well-known/oauth-protected-resource/mcp":
		value := struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
		}{base + "/mcp", []string{base + "/"}}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			http.Error(w, "fixture encoding failed", 500)
		}
	case "/v1/enroll":
		f.enroll(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *upgradeEnrollment) enroll(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token        string `json:"token"`
		CSR          string `json:"csr_der_base64"`
		RequireOwned bool   `json:"require_owned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&request); err != nil || !request.RequireOwned {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	der, err := base64.StdEncoding.DecodeString(request.CSR)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now, NotAfter: now.Add(30 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true, URIs: []*url.URL{{Scheme: "spiffe", Host: "delegation-layer", Path: "/device/upgrade-device"}, {Scheme: "urn", Opaque: "delegation-layer:credential:upgrade-epoch:1"}}}
	certificate, err := x509.CreateCertificate(rand.Reader, template, f.ca, csr.PublicKey, f.key)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.requests.Add(1)
	response := struct {
		Device      string `json:"device_id"`
		Epoch       string `json:"epoch"`
		Generation  uint64 `json:"generation"`
		Mode        string `json:"mode"`
		Certificate string `json:"certificate_der_base64"`
	}{"upgrade-device", "upgrade-epoch", 1, "regular", base64.StdEncoding.EncodeToString(certificate)}
	if err = json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "fixture encoding failed", 500)
	}
}

type upgradeFixture struct {
	root          string
	profile       string
	rootToken     string
	oldExecutable string
	newExecutable string
	enrollment    *upgradeEnrollment
	dependencies  Dependencies
	client        *upgradeClient
	native        *upgradeNative
	services      *upgradeServices
	history       map[string][]byte
	identity      map[string][]byte
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	root, observed, err := rootContext(boundedTestContext(t), filepath.Join(t.TempDir(), "state"), predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	f := &upgradeFixture{root: root, profile: filepath.Join(root, "gateway"), rootToken: observed.RootToken, enrollment: newUpgradeEnrollment(t)}
	base := t.TempDir()
	f.oldExecutable, f.newExecutable = filepath.Join(base, "old-keg", "delegate"), filepath.Join(base, "new-keg", "delegate")
	for _, path := range []string{f.oldExecutable, f.newExecutable} {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte("#!/bin/sh\nexit 64\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	caPath := filepath.Join(base, "server-ca.pem")
	if err = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.enrollment.server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	token := struct {
		Token  string    `json:"token"`
		Device string    `json:"device_id"`
		Expiry time.Time `json:"expires_at"`
	}{base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "upgrade-device", time.Now().Add(time.Hour)}
	tokenBytes, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	options := deviceclient.ConnectOptions{GatewayURL: f.enrollment.server.URL, ProfileDirectory: f.profile, DelegateExecutable: f.oldExecutable, DelegateRoot: f.root, DelegateRootContext: f.rootToken, DeviceName: "upgrade-fixture", PrivateCAFile: caPath, EnrollmentTokenReader: bytes.NewReader(tokenBytes), Workspaces: []deviceclient.WorkspaceApproval{{Alias: "approved", Directory: workspace, NativeContext: "fixture-workspace-token"}}}
	if _, err = deviceclient.Connect(boundedTestContext(t), options); err != nil {
		t.Fatal(err)
	}
	workspaces, err := deviceclient.ListWorkspaces(t.Context(), f.profile)
	if err != nil || len(workspaces) != 1 {
		t.Fatalf("workspace setup failed: %v", err)
	}
	if _, err = deviceclient.RemoveWorkspace(t.Context(), f.profile, workspaces[0].ID, workspaces[0].Revision); err != nil {
		t.Fatal(err)
	}
	makeHistoricalUpgradeProfile(t, f.profile)
	f.history = upgradeTaskHistory(t, f.root)
	f.identity = upgradeTreeSnapshot(t, filepath.Join(f.profile, "identity"))
	f.client, f.native, f.services = &upgradeClient{}, &upgradeNative{root: f.root, rootToken: f.rootToken}, &upgradeServices{}
	f.dependencies = commandDependencies(t, f.client)
	f.dependencies.Executable, f.dependencies.Services, f.dependencies.NativeInstallation = f.newExecutable, f.services, f.native
	return f
}

func upgradeTaskHistory(t *testing.T, root string) map[string][]byte {
	t.Helper()
	store, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	testsupport.NewAppTaskInStore(t, store, strings.Repeat("c", 32), false, nil)
	history := upgradeTreeSnapshot(t, filepath.Join(root, "tasks"))
	rootPath := filepath.Join(root, "root.json")
	rootBytes, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	history[rootPath] = rootBytes
	return history
}

// Historical bytes are test input only. Product migration uses the public SDK
// owner, never this fixture encoder or direct profile/credential edits.
func makeHistoricalUpgradeProfile(t *testing.T, directory string) {
	t.Helper()
	path := filepath.Join(directory, "profile.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["schema_version"] = json.RawMessage(`2`)
	document["providers"] = json.RawMessage(`[{"provider":"private-legacy-policy-canary","permissions":["read-only"],"max_budget_seconds":1}]`)
	var workspaces []map[string]json.RawMessage
	if err = json.Unmarshal(document["workspaces"], &workspaces); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range workspaces {
		workspace["providers"] = document["providers"]
	}
	document["workspaces"], err = json.Marshal(workspaces)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func upgradeTreeSnapshot(t *testing.T, directory string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() || strings.HasSuffix(path, ".lock") {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err == nil {
			files[path] = data
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func (f *upgradeFixture) assertPreserved(t *testing.T) {
	t.Helper()
	for path, want := range f.identity {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("upgrade changed credential evidence %s: %v", filepath.Base(path), err)
		}
	}
	for path, want := range f.history {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("upgrade changed task history %s: %v", filepath.Base(path), err)
		}
	}
	if f.enrollment.requests.Load() != 1 {
		t.Fatal("upgrade re-enrolled the device")
	}
}

func (f *upgradeFixture) run(t *testing.T, preview bool) (Response, int) {
	t.Helper()
	var output, diagnostic bytes.Buffer
	code := Run(boundedTestContext(t), Arguments{Action: "upgrade", Profile: f.profile, Preview: preview, AcceptExecutionPolicyRemoval: !preview}, f.root, true, &output, &diagnostic, f.dependencies)
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("upgrade exit%d could not decode response: %v", code, err)
	}
	if bytes.Contains(output.Bytes(), []byte(f.rootToken)) || bytes.Contains(output.Bytes(), []byte("private-legacy-policy-canary")) {
		t.Fatal("upgrade response exposed private source or policy contents")
	}
	return response, code
}

var errUpgradeFixture = errors.New("upgrade fixture interruption")
