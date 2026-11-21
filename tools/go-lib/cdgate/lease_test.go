package cdgate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// fixtureFiles serves the three service-account files from a map.
func fixtureFiles(files map[string]string) kubeserviceaccount.ReadFile {
	return func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(value), nil
	}
}

func leaseEnv(m map[string]string) LeaseEnv {
	return func(key string) string { return m[key] }
}

// completeFiles is a full service account. Its CA is a fresh certificate, so the
// production client can parse its pool without a fixture that goes stale.
func completeFiles(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		kubeserviceaccount.TokenPath:     "token-value\n",
		kubeserviceaccount.NamespacePath: "woodpecker\n",
		kubeserviceaccount.CAPath:        testCAPEM(t),
	}
}

var completeEnv = map[string]string{
	"KUBERNETES_SERVICE_HOST": "10.0.0.1",
	"KUBERNETES_SERVICE_PORT": "443",
	"CI_PIPELINE_NUMBER":      "42",
	"CI_STEP_NAME":            "pulumi-up-aws",
	"HOSTNAME":                "pod-1",
	"CD_LEASE_NAME":           "cd-lease",
}

func TestCreateLeaseClientIsNilOutsideKubernetes(t *testing.T) {
	cfg, err := CreateKubernetesLeaseClient(leaseEnv(map[string]string{}), fixtureFiles(nil), nil)
	if !errors.Is(err, kubeserviceaccount.ErrNotKubernetes) || cfg != nil {
		t.Fatalf("got %v, %v; want nil, kubeserviceaccount.ErrNotKubernetes outside Kubernetes", cfg, err)
	}
}

func TestCreateLeaseClientNamesTheMissingToken(t *testing.T) {
	_, err := CreateKubernetesLeaseClient(leaseEnv(completeEnv), fixtureFiles(map[string]string{
		kubeserviceaccount.NamespacePath: "woodpecker", kubeserviceaccount.CAPath: "x",
	}), nil)
	if err == nil {
		t.Fatal("accepted a missing token")
	}
	if !strings.Contains(err.Error(), "ALLOW_FROM_STEP") {
		t.Fatalf("error %q does not name the step service-account check", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %q does not wrap the missing file", err)
	}
}

func TestCreateLeaseClientRejectsEmptyNamespace(t *testing.T) {
	files := map[string]string{
		kubeserviceaccount.TokenPath:     "t",
		kubeserviceaccount.NamespacePath: "   \n",
		kubeserviceaccount.CAPath:        "x",
	}
	_, err := CreateKubernetesLeaseClient(leaseEnv(completeEnv), fixtureFiles(files), nil)
	if err == nil || !strings.Contains(err.Error(), "empty file") {
		t.Fatalf("err = %v, want an empty-file error for the namespace", err)
	}
}

func TestCreateLeaseClientRequiresStepEnv(t *testing.T) {
	env := make(map[string]string, len(completeEnv))
	maps.Copy(env, completeEnv)
	delete(env, "CI_STEP_NAME")
	_, err := CreateKubernetesLeaseClient(leaseEnv(env), fixtureFiles(completeFiles(t)), nil)
	if err == nil || !strings.Contains(err.Error(), "CI_STEP_NAME") {
		t.Fatalf("err = %v, want a missing CI_STEP_NAME error", err)
	}
}

func TestCreateLeaseClientBuildsHolderIdentity(t *testing.T) {
	cfg, err := CreateKubernetesLeaseClient(leaseEnv(completeEnv), fixtureFiles(completeFiles(t)), &http.Client{})
	if err != nil {
		t.Fatalf("CreateKubernetesLeaseClient: %v", err)
	}
	if cfg.Name != "cd-lease" || cfg.HolderIdentity != "42/pulumi-up-aws/pod-1" {
		t.Fatalf("cfg = %+v, want name cd-lease and holder 42/pulumi-up-aws/pod-1", cfg)
	}
}

// decodeLease runs the production decode path: the JSON body into the API
// response shape, then into the record the gate reads.
func decodeLease(raw string) (*LeaseRecord, error) {
	var resp leaseResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, err
	}
	return leaseRecordOf(resp)
}

func TestParseLeaseReadsOptionalFields(t *testing.T) {
	rec, err := decodeLease(`{"metadata":{"resourceVersion":"7"},"spec":{"holderIdentity":"h","renewTime":"r","leaseTransitions":3}}`)
	if err != nil {
		t.Fatalf("decodeLease: %v", err)
	}
	if rec.ResourceVersion != "7" || rec.HolderIdentity != "h" || rec.RenewTime != "r" || rec.LeaseTransitions != 3 {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestParseLeaseRejectsMissingResourceVersion(t *testing.T) {
	if _, err := decodeLease(`{"metadata":{},"spec":{}}`); err == nil {
		t.Fatal("accepted a Lease with no resourceVersion")
	}
}

// fakeLeaseAPI serves one Lease response.
func fakeLeaseAPI(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testLeases builds the production Lease client against a test server. It goes
// through the same constructor production uses, with the API base injected.
func testLeases(t *testing.T, srv *httptest.Server, files map[string]string) *kubeLeases {
	t.Helper()
	cluster, err := kubeserviceaccount.Connect(kubeserviceaccount.Options{
		Env:  func(string) string { return "" },
		Read: fixtureFiles(files),
		HTTP: srv.Client(),
		Base: srv.URL,
	})
	if err != nil {
		t.Fatalf("kubeserviceaccount.Connect: %v", err)
	}
	return &kubeLeases{cluster: cluster}
}

func TestGetReportsMissingLeaseAsNil(t *testing.T) {
	srv := fakeLeaseAPI(t, http.StatusNotFound, `{}`)
	client := testLeases(t, srv, completeFiles(t))
	rec, err := client.Get(context.Background(), "cd-lease")
	if rec != nil || !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("Get = %v, %v; want ErrLeaseNotFound for a missing Lease", rec, err)
	}
}

func TestGetReturnsStatusErrorForConflict(t *testing.T) {
	srv := fakeLeaseAPI(t, http.StatusConflict, `{}`)
	client := testLeases(t, srv, completeFiles(t))
	_, err := client.Update(context.Background(), LeaseWrite{Name: "cd-lease"})
	status, ok := errors.AsType[*kubeserviceaccount.StatusError](err)
	if !ok || status.Status != http.StatusConflict {
		t.Fatalf("err = %v, want StatusError 409", err)
	}
}

func TestUpdateSendsBearerTokenAndResourceVersion(t *testing.T) {
	body := `{"metadata":{"resourceVersion":"8"},"spec":{"holderIdentity":"42/s/pod-1"}}`
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := testLeases(t, srv, completeFiles(t))
	rec, err := client.Update(context.Background(), LeaseWrite{
		Name: "cd-lease", ResourceVersion: "7", HolderIdentity: "42/s/pod-1", LeaseDurationSeconds: 120,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if gotAuth != "Bearer token-value" {
		t.Fatalf("Authorization = %q, want the trimmed service-account token", gotAuth)
	}
	if !strings.Contains(gotBody, `"resourceVersion":"7"`) {
		t.Fatalf("body %s does not carry the resourceVersion", gotBody)
	}
	if rec.ResourceVersion != "8" {
		t.Fatalf("resourceVersion = %q, want 8", rec.ResourceVersion)
	}
}

func TestUpdateOmitsEmptyAcquireTime(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		gotBody = string(raw)
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"metadata":{"resourceVersion":"8"},"spec":{"holderIdentity":""}}`); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	client := testLeases(t, srv, completeFiles(t))
	_, err := client.Update(context.Background(), LeaseWrite{Name: "cd-lease", ResourceVersion: "7"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if strings.Contains(gotBody, `"acquireTime":""`) {
		t.Fatalf("request body contains an empty acquireTime: %s", gotBody)
	}
}

// testCAPEM returns a fresh self-signed certificate in PEM form, so the client
// can parse its CA pool without a fixture that goes stale. A key or certificate
// failure is a test-setup failure, reported through the test, not a panic.
func testCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cdgate-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test CA certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
