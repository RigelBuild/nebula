package cdgate

import (
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// TestKubernetesLeaseClientTrustsServiceAccountCA: the production constructor
// builds its own TLS client from the mounted CA file. A Lease API served with a
// certificate from that CA must be reachable through it. Production passes a nil
// doer, so the CA pool is the only trust path.
func TestKubernetesLeaseClientTrustsServiceAccountCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"kind":"Status"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	files := map[string]string{
		kubeserviceaccount.TokenPath:     "token-value",
		kubeserviceaccount.NamespacePath: "ns",
		kubeserviceaccount.CAPath:        string(caPEM),
	}
	read := func(path string) ([]byte, error) {
		v, ok := files[path]
		if !ok {
			return nil, errors.New("no such file")
		}
		return []byte(v), nil
	}
	env := map[string]string{
		"KUBERNETES_SERVICE_HOST": host,
		"KUBERNETES_SERVICE_PORT": port,
		"CI_PIPELINE_NUMBER":      "7",
		"CI_STEP_NAME":            "deploy",
		"HOSTNAME":                "pod-1",
		"CD_LEASE_NAME":           "cd-lease",
	}
	cfg, err := CreateKubernetesLeaseClient(func(k string) string { return env[k] }, read, nil)
	if err != nil {
		t.Fatalf("CreateKubernetesLeaseClient: %v", err)
	}
	if cfg == nil {
		t.Fatal("constructor returned no client for a Kubernetes step")
	}
	// A TLS failure here means the CA pool was not used. A 404 means the TLS
	// handshake succeeded and the Lease is absent.
	rec, err := cfg.Client.Get(context.Background(), "cd-lease")
	if !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("Get through production client: %v; want ErrLeaseNotFound after trusted TLS", err)
	}
	if rec != nil {
		t.Fatalf("Get = %+v, want no record for a missing Lease", rec)
	}
}
