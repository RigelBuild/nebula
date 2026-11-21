// Package reconcile converges one Garage bucket's quota and key grant.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/garage"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/jscompat"
)

// Config is the reconcile input read from the environment.
type Config struct {
	AdminURL    string
	AdminToken  string
	AccessKeyID string
	BucketName  string
	MaxSize     int64
	// ReadyWait bounds the admin readiness poll. Zero means garage.DefaultReadyTimeout.
	ReadyWait time.Duration
}

type bucketEntry struct {
	id      string
	aliases []string
}

// Run waits for the admin API, then ensures the bucket, quota, and key grant exist.
func Run(ctx context.Context, cfg Config, deps garage.Deps) error {
	deps.Log("Waiting for Garage admin API readiness")
	if err := garage.WaitForAdmin(ctx, cfg.AdminURL, readyWait(cfg.ReadyWait), deps); err != nil {
		return err
	}

	deps.Log("Listing Garage buckets")
	listed, err := call(ctx, cfg, deps, "ListBuckets", http.MethodGet, nil)
	if err != nil {
		return err
	}
	buckets, ok := listedBuckets(listed)
	if !ok {
		return errors.New("ListBuckets response did not contain valid bucket entries")
	}
	id, found := findBucket(buckets, cfg.BucketName)
	if !found {
		deps.Log("Creating Garage bucket " + cfg.BucketName)
		created, err := call(ctx, cfg, deps, "CreateBucket", http.MethodPost, map[string]any{"globalAlias": cfg.BucketName})
		if err != nil {
			return err
		}
		if id, err = bucketID(created, "CreateBucket"); err != nil {
			return err
		}
	}

	deps.Log("Checking Garage access key")
	keyInfo, err := call(ctx, cfg, deps, "GetKeyInfo?id="+jscompat.EncodeURIComponent(cfg.AccessKeyID), http.MethodGet, nil)
	if err != nil {
		return err
	}
	if !accessKeyMatches(keyInfo, cfg.AccessKeyID) {
		return errors.New("GetKeyInfo response accessKeyId did not match the requested key")
	}

	deps.Log("Setting Garage bucket quota")
	updated, err := call(ctx, cfg, deps, "UpdateBucket?id="+jscompat.EncodeURIComponent(id), http.MethodPost,
		map[string]any{"quotas": map[string]any{"maxSize": cfg.MaxSize, "maxObjects": nil}})
	if err != nil {
		return err
	}
	if !quotaConfirmed(updated, cfg.MaxSize) {
		return errors.New("UpdateBucket response did not confirm the requested maxSize")
	}

	deps.Log("Granting Garage bucket key access")
	granted, err := call(ctx, cfg, deps, "AllowBucketKey", http.MethodPost, map[string]any{
		"bucketId":    id,
		"accessKeyId": cfg.AccessKeyID,
		"permissions": map[string]any{"read": true, "write": true, "owner": false},
	})
	if err != nil {
		return err
	}
	if !grantConfirmed(granted, cfg.AccessKeyID) {
		return errors.New("AllowBucketKey response did not confirm read/write access without owner")
	}
	return nil
}

// readyWait returns the configured readiness timeout, or garage.DefaultReadyTimeout when unset.
func readyWait(configured time.Duration) time.Duration {
	if configured == 0 {
		return garage.DefaultReadyTimeout
	}
	return configured
}

// call sends one admin request and decodes its JSON body. Transport errors pass through unchanged.
func call(ctx context.Context, cfg Config, deps garage.Deps, endpoint, method string, body any) (any, error) {
	status, text, err := garage.Send(ctx, deps, cfg.AdminURL, endpoint, method, cfg.AdminToken, body)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s returned HTTP %d: %s", endpoint, status, garage.ErrorBody(string(text), cfg.AdminToken))
	}
	return garage.DecodeJSON(endpoint, text)
}

// listedBuckets validates every entry and returns them as typed values.
func listedBuckets(value any) ([]bucketEntry, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]bucketEntry, 0, len(items))
	for _, item := range items {
		record, ok := garage.Record(item)
		if !ok {
			return nil, false
		}
		id, idOK := record["id"].(string)
		aliases, aliasesOK := garage.StringArray(record["globalAliases"])
		if !idOK || !aliasesOK {
			return nil, false
		}
		out = append(out, bucketEntry{id: id, aliases: aliases})
	}
	return out, true
}

// findBucket returns the first bucket whose global aliases include name.
func findBucket(buckets []bucketEntry, name string) (string, bool) {
	for _, bucket := range buckets {
		if slices.Contains(bucket.aliases, name) {
			return bucket.id, true
		}
	}
	return "", false
}

// bucketID reads the id from a CreateBucket response.
func bucketID(value any, endpoint string) (string, error) {
	if record, ok := garage.Record(value); ok {
		if id, ok := record["id"].(string); ok {
			return id, nil
		}
	}
	return "", fmt.Errorf("%s response did not include a bucket id", endpoint)
}

// accessKeyMatches reports whether GetKeyInfo or an AllowBucketKey entry echoes the requested key.
func accessKeyMatches(value any, accessKeyID string) bool {
	record, ok := garage.Record(value)
	if !ok {
		return false
	}
	got, ok := record["accessKeyId"].(string)
	return ok && got == accessKeyID
}

// quotaConfirmed reports whether UpdateBucket echoes the requested maxSize.
func quotaConfirmed(value any, maxSize int64) bool {
	record, ok := garage.Record(value)
	if !ok {
		return false
	}
	quotas, ok := garage.Record(record["quotas"])
	if !ok {
		return false
	}
	got, ok := quotas["maxSize"].(float64)
	return ok && got == float64(maxSize)
}

// grantConfirmed reports read and write without owner for the requested key.
func grantConfirmed(value any, accessKeyID string) bool {
	record, ok := garage.Record(value)
	if !ok {
		return false
	}
	keys, ok := record["keys"].([]any)
	if !ok {
		return false
	}
	for _, candidate := range keys {
		key, ok := garage.Record(candidate)
		if !ok || !accessKeyMatches(key, accessKeyID) {
			continue
		}
		permissions, ok := garage.Record(key["permissions"])
		if !ok {
			return false
		}
		return permissions["read"] == true && permissions["write"] == true && permissions["owner"] != true
	}
	return false
}
