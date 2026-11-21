// Command garage-bucket-reconcile converges one Garage bucket: its size quota and the read/write grant for one access key.
//
// Env: GARAGE_ADMIN_URL, GARAGE_ADMIN_TOKEN, S3_ACCESS_KEY_ID, BUCKET_NAME, BUCKET_MAX_SIZE_BYTES.
// Exit codes: 0 success; 1 any failure.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/garage"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/jscompat"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/reconcile"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, &http.Client{}, os.Stdout, os.Stderr))
}

// run is the testable body. argv is ignored; the command takes no arguments.
// A failed write to stdout or stderr is itself a failure: the run's output is lost, so exit 1.
func run(ctx context.Context, _ []string, getenv func(string) string, httpClient *http.Client, stdout, stderr io.Writer) int {
	writeErr := error(nil)
	fail := func(msg string) int {
		// Discarded on purpose: stderr is the only sink left, and exit 1 already reports the failure.
		_, _ = fmt.Fprintln(stderr, msg)
		return 1
	}
	cfg, err := readConfig(getenv)
	if err != nil {
		return fail(err.Error())
	}
	deps := garage.Deps{
		HTTP:  httpClient,
		Sleep: garage.Sleep,
		Now:   time.Now,
		Log: func(line string) {
			if _, err := fmt.Fprintln(stdout, line); err != nil && writeErr == nil {
				writeErr = err
			}
		},
	}
	if err := reconcile.Run(ctx, cfg, deps); err != nil {
		return fail(err.Error())
	}
	if writeErr != nil {
		return fail("writing reconcile output: " + writeErr.Error())
	}
	return 0
}

// readConfig reads env in a fixed order, so the first missing variable is the one reported.
func readConfig(getenv func(string) string) (reconcile.Config, error) {
	rawMaxSize, err := jscompat.Required(getenv, "BUCKET_MAX_SIZE_BYTES")
	if err != nil {
		return reconcile.Config{}, err
	}
	maxSize := jscompat.Number(rawMaxSize)
	if !jscompat.IsSafeInteger(maxSize) || maxSize < 0 {
		return reconcile.Config{}, errMaxSize
	}
	adminURL, err := jscompat.Required(getenv, "GARAGE_ADMIN_URL")
	if err != nil {
		return reconcile.Config{}, err
	}
	adminToken, err := jscompat.Required(getenv, "GARAGE_ADMIN_TOKEN")
	if err != nil {
		return reconcile.Config{}, err
	}
	accessKeyID, err := jscompat.Required(getenv, "S3_ACCESS_KEY_ID")
	if err != nil {
		return reconcile.Config{}, err
	}
	bucketName, err := jscompat.Required(getenv, "BUCKET_NAME")
	if err != nil {
		return reconcile.Config{}, err
	}
	return reconcile.Config{
		AdminURL:    garage.TrimTrailingSlashes(adminURL),
		AdminToken:  adminToken,
		AccessKeyID: accessKeyID,
		BucketName:  bucketName,
		MaxSize:     int64(maxSize),
	}, nil
}

var errMaxSize = errors.New("BUCKET_MAX_SIZE_BYTES must be a non-negative safe integer")
