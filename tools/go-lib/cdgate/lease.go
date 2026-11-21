package cdgate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// ErrLeaseNotFound means the Lease does not exist yet, so the caller can create it.
var ErrLeaseNotFound = errors.New("kubernetes lease not found")

// LeaseEnv is the environment the Lease client reads. Tests pass a map.
type LeaseEnv func(string) string

// LeaseRecord is the state of one Kubernetes Lease as the CD gate reads it.
type LeaseRecord struct {
	ResourceVersion  string
	HolderIdentity   string
	RenewTime        string
	AcquireTime      string
	LeaseTransitions int64
}

// LeaseWrite is what the gate sends when it creates or updates a Lease.
type LeaseWrite struct {
	Name                 string
	ResourceVersion      string
	LeaseDurationSeconds int64
	HolderIdentity       string
	RenewTime            string
	AcquireTime          string
	LeaseTransitions     int64
}

// LeaseClient reads and writes one Lease. ErrLeaseNotFound from Get means the
// caller can create it.
type LeaseClient interface {
	Get(ctx context.Context, name string) (*LeaseRecord, error)
	Create(ctx context.Context, lease LeaseWrite) (*LeaseRecord, error)
	Update(ctx context.Context, lease LeaseWrite) (*LeaseRecord, error)
}

// LeaseConfig is a ready Lease client plus the identity this step writes as
// holder: pipeline/step/pod.
type LeaseConfig struct {
	Name           string
	HolderIdentity string
	Client         LeaseClient
}

// CreateKubernetesLeaseClient builds the Lease client for a Kubernetes step. It
// returns kubeserviceaccount.ErrNotKubernetes when KUBERNETES_SERVICE_HOST is
// unset, so the caller runs the command without a Lease. It fails at setup when
// service-account configuration or the lease-holder environment is missing or empty.
func CreateKubernetesLeaseClient(env LeaseEnv, read kubeserviceaccount.ReadFile, doer *http.Client) (*LeaseConfig, error) {
	return createKubernetesLeaseClient(env, read, doer, "")
}

// createKubernetesLeaseClient is CreateKubernetesLeaseClient with an injectable
// API base. Tests set base to an httptest server; production leaves it empty.
func createKubernetesLeaseClient(env LeaseEnv, read kubeserviceaccount.ReadFile, doer *http.Client, base string) (*LeaseConfig, error) {
	cluster, err := kubeserviceaccount.Connect(kubeserviceaccount.Options{
		Env:  kubeserviceaccount.Env(env),
		Read: read,
		HTTP: doer,
		Base: base,
	})
	if err != nil {
		return nil, err
	}
	pipeline, err := requiredEnv(env, "CI_PIPELINE_NUMBER")
	if err != nil {
		return nil, err
	}
	step, err := requiredEnv(env, "CI_STEP_NAME")
	if err != nil {
		return nil, err
	}
	pod, err := requiredEnv(env, "HOSTNAME")
	if err != nil {
		return nil, err
	}
	leaseName, err := requiredEnv(env, "CD_LEASE_NAME")
	if err != nil {
		return nil, err
	}
	return &LeaseConfig{
		Name:           leaseName,
		HolderIdentity: fmt.Sprintf("%s/%s/%s", pipeline, step, pod),
		Client:         &kubeLeases{cluster: cluster},
	}, nil
}

// requiredEnv reads one lease-holder variable. The service-account variables are
// read by kubeserviceaccount; this covers only the names that identify the holder.
func requiredEnv(env LeaseEnv, name string) (string, error) {
	value := strings.TrimSpace(env(name))
	if value == "" {
		return "", fmt.Errorf("missing %s", name)
	}
	return value, nil
}

// kubeLeases talks to the coordination.k8s.io Lease API through the shared
// service-account client, which re-reads the rotated bearer token per request.
type kubeLeases struct {
	cluster *kubeserviceaccount.Cluster
}

const leasesPath = "/apis/coordination.k8s.io/v1/namespaces/"

type leaseBody struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   leaseMetadata `json:"metadata"`
	Spec       leaseSpec     `json:"spec"`
}

type leaseMetadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type leaseSpec struct {
	LeaseDurationSeconds int64  `json:"leaseDurationSeconds"`
	HolderIdentity       string `json:"holderIdentity"`
	RenewTime            string `json:"renewTime"`
	AcquireTime          string `json:"acquireTime,omitempty"`
	LeaseTransitions     int64  `json:"leaseTransitions"`
}

type leaseResponse struct {
	Metadata struct {
		ResourceVersion *string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		HolderIdentity   *string `json:"holderIdentity"`
		RenewTime        *string `json:"renewTime"`
		AcquireTime      *string `json:"acquireTime"`
		LeaseTransitions *int64  `json:"leaseTransitions"`
	} `json:"spec"`
}

// leaseRecordOf maps one decoded API response to the record the gate reads. A
// response without a resourceVersion is malformed: the gate could not write
// conditionally against it.
func leaseRecordOf(resp leaseResponse) (*LeaseRecord, error) {
	if resp.Metadata.ResourceVersion == nil {
		return nil, errors.New("invalid Kubernetes lease fields")
	}
	rec := &LeaseRecord{ResourceVersion: *resp.Metadata.ResourceVersion}
	if resp.Spec.HolderIdentity != nil {
		rec.HolderIdentity = *resp.Spec.HolderIdentity
	}
	if resp.Spec.RenewTime != nil {
		rec.RenewTime = *resp.Spec.RenewTime
	}
	if resp.Spec.AcquireTime != nil {
		rec.AcquireTime = *resp.Spec.AcquireTime
	}
	if resp.Spec.LeaseTransitions != nil {
		rec.LeaseTransitions = *resp.Spec.LeaseTransitions
	}
	return rec, nil
}

func (k *kubeLeases) Get(ctx context.Context, name string) (*LeaseRecord, error) {
	var resp leaseResponse
	err := k.cluster.Do(ctx, http.MethodGet, k.collection()+"/"+kubeserviceaccount.EscapePath(name), nil, &resp)
	if kubeserviceaccount.NotFound(err) {
		return nil, ErrLeaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return leaseRecordOf(resp)
}

func (k *kubeLeases) Create(ctx context.Context, lease LeaseWrite) (*LeaseRecord, error) {
	return k.write(ctx, http.MethodPost, k.collection(), lease)
}

func (k *kubeLeases) Update(ctx context.Context, lease LeaseWrite) (*LeaseRecord, error) {
	return k.write(ctx, http.MethodPut, k.collection()+"/"+kubeserviceaccount.EscapePath(lease.Name), lease)
}

func (k *kubeLeases) write(ctx context.Context, method, path string, lease LeaseWrite) (*LeaseRecord, error) {
	body := leaseBody{
		APIVersion: "coordination.k8s.io/v1",
		Kind:       "Lease",
		Metadata: leaseMetadata{
			Name:            lease.Name,
			Namespace:       k.cluster.Namespace(),
			ResourceVersion: lease.ResourceVersion,
		},
		Spec: leaseSpec{
			LeaseDurationSeconds: lease.LeaseDurationSeconds,
			HolderIdentity:       lease.HolderIdentity,
			RenewTime:            lease.RenewTime,
			AcquireTime:          lease.AcquireTime,
			LeaseTransitions:     lease.LeaseTransitions,
		},
	}
	var resp leaseResponse
	if err := k.cluster.Do(ctx, method, path, body, &resp); err != nil {
		return nil, err
	}
	return leaseRecordOf(resp)
}

func (k *kubeLeases) collection() string {
	return leasesPath + kubeserviceaccount.EscapePath(k.cluster.Namespace()) + "/leases"
}
