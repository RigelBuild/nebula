// SPDX-License-Identifier: MIT OR Apache-2.0

package cdrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2" //nolint:depguard // backoff jitter, not security
	"net/http"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
	"github.com/RigelBuild/nebula/tools/go-lib/transientretry"
)

// QueueConfigMap is the ConfigMap that holds the release queue state. The
// cluster RBAC rule must name the same object.
const QueueConfigMap = "cd-release-queue"

// QueueSeedState is the state the ConfigMap is created with. Flux seeds it once
// (IfNotPresent) and never overwrites it afterwards.
const QueueSeedState = `{"pending":[],"failed":{}}`

// writeAttempts bounds one Update: a 409 re-reads, and a transient error retries,
// up to this many PUTs before the write fails loud.
const writeAttempts = 5

// writeBaseWait is the first backoff between two write attempts; it doubles on
// each retry, up to writeMaxWait.
const (
	writeBaseWait = 100 * time.Millisecond
	writeMaxWait  = 2 * time.Second
)

// Store reads the queue state, applies fn, and writes the result back as one
// compare-and-swap on resourceVersion.
type Store interface {
	// Update reads the state, applies fn, and CAS-writes the result. A 409 means
	// another writer won: Update re-reads and re-applies fn. A 5xx, 429 or
	// transport error retries with bounded backoff. Any other API error fails
	// at once. Every failed attempt is logged. When the bound is spent, Update
	// returns the error; it never falls back to an unchecked write. An fn that
	// returns the state it read sends no PUT.
	Update(ctx context.Context, fn func(State) (State, error)) (State, error)
}

// configMap is the ConfigMap the store reads and writes. Metadata is kept as the
// API returned it: a PUT replaces the whole object, so dropping any field (labels,
// finalizers, owner references) would change the object Flux manages.
type configMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   map[string]any    `json:"metadata"`
	Data       map[string]string `json:"data"`
}

// resourceVersion returns metadata.resourceVersion, or "" when absent.
func (c configMap) resourceVersion() string {
	rv, _ := c.Metadata["resourceVersion"].(string)
	return rv
}

type configMapStore struct {
	cluster  *kubeserviceaccount.Cluster
	name     string
	attempts int
	sleep    transientretry.Sleeper
}

// NewConfigMapStore builds the store from the step pod's environment and
// service-account files. A nil doer uses the mounted CA. It fails at setup when
// the environment is not a Kubernetes step.
func NewConfigMapStore(env func(string) string, read func(string) ([]byte, error), doer *http.Client) (Store, error) {
	cluster, err := kubeserviceaccount.Connect(kubeserviceaccount.Options{Env: env, Read: read, HTTP: doer})
	if err != nil {
		return nil, fmt.Errorf("cd release queue store: %w", err)
	}
	return newConfigMapStore(cluster, writeAttempts), nil
}

func newConfigMapStore(cluster *kubeserviceaccount.Cluster, attempts int) Store {
	return &configMapStore{cluster: cluster, name: QueueConfigMap, attempts: attempts, sleep: transientretry.SleepCtx}
}

func (s *configMapStore) Update(ctx context.Context, fn func(State) (State, error)) (State, error) {
	var out State
	op := func(ctx context.Context) error {
		state, err := s.attempt(ctx, fn)
		if err != nil {
			return err
		}
		out = state
		return nil
	}
	bounds := transientretry.Bounds{Attempts: s.attempts, BaseWait: writeBaseWait, MaxWait: writeMaxWait}
	err := transientretry.Do(ctx, op, classifyWrite, bounds, jittered(s.sleep))
	if err == nil {
		return out, nil
	}
	if _, refused := errors.AsType[refusal](err); refused {
		return State{}, err
	}
	slog.Default().Warn("cd release queue write failed",
		"attempts", s.attempts,
		"error", err.Error())
	return State{}, fmt.Errorf("cd release queue write failed after %d attempts: %w", s.attempts, err)
}

// classifyWrite retries a 409 (another writer won, so re-read), a 5xx or 429, and
// a transport failure. Any other status is permanent: it will not heal on retry.
func classifyWrite(err error) transientretry.Class {
	if _, refused := errors.AsType[refusal](err); refused {
		return ""
	}
	if kubeserviceaccount.Conflict(err) {
		return transientretry.ExternalHTTPTransient
	}
	if status, ok := errors.AsType[*kubeserviceaccount.StatusError](err); ok {
		if status.Status == http.StatusTooManyRequests || (status.Status >= 500 && status.Status <= 599) {
			return transientretry.ExternalHTTPTransient
		}
		return ""
	}
	return transientretry.ClassifyExternalHTTP(err)
}

// jittered returns a Sleeper that waits d plus up to d/2 of random jitter, never
// more than writeMaxWait. Jitter spreads retries that collided under contention.
func jittered(sleep transientretry.Sleeper) transientretry.Sleeper {
	return func(ctx context.Context, d time.Duration) error {
		extra := time.Duration(rand.Int64N(int64(d)/2 + 1)) //nolint:gosec // backoff jitter, not security
		return sleep(ctx, min(d+extra, writeMaxWait))
	}
}

func (s *configMapStore) path() string {
	return "/api/v1/namespaces/" + kubeserviceaccount.EscapePath(s.cluster.Namespace()) +
		"/configmaps/" + kubeserviceaccount.EscapePath(s.name)
}

// refusal marks an error from fn. Update returns it without retrying: the
// caller decided not to write, so a retry would ask the same question again.
type refusal struct{ err error }

func (r refusal) Error() string { return r.err.Error() }
func (r refusal) Unwrap() error { return r.err }

// attempt runs one read-apply-write cycle. It sends back the ConfigMap as read,
// changing only data.state, and sends no PUT when fn returns the state unchanged.
func (s *configMapStore) attempt(ctx context.Context, fn func(State) (State, error)) (State, error) {
	var cm configMap
	if err := s.cluster.Do(ctx, http.MethodGet, s.path(), nil, &cm); err != nil {
		return State{}, fmt.Errorf("read %s: %w", s.name, err)
	}
	if cm.resourceVersion() == "" {
		return State{}, errors.New("read cd release queue: no resourceVersion; refusing an unconditional write")
	}
	var current State
	raw := cm.Data["state"]
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return State{}, fmt.Errorf("decode cd release queue state: %w", err)
		}
	}
	next, err := fn(current)
	if err != nil {
		return State{}, refusal{err: err}
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return State{}, fmt.Errorf("encode cd release queue state: %w", err)
	}
	if string(encoded) == raw {
		return next, nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["state"] = string(encoded)
	if err := s.cluster.Do(ctx, http.MethodPut, s.path(), cm, nil); err != nil {
		return State{}, fmt.Errorf("write %s: %w", s.name, err)
	}
	return next, nil
}
