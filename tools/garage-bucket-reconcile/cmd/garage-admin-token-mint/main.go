// Command garage-admin-token-mint mints and publishes the scoped Garage admin token into a Kubernetes Secret.
//
// Env: GARAGE_ADMIN_URL, GARAGE_MASTER_TOKEN, POD_NAMESPACE, SECRET_NAME, TOKEN_NAME, plus the
// in-cluster KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT.
// Exit codes: 0 success; 1 any failure.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/garage"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/jscompat"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/kube"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/mint"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, &http.Client{}, os.Stdout, os.Stderr))
}

// run is the testable body. argv is ignored; the command takes no arguments.
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
	secrets, err := kube.New(getenv)
	if err != nil {
		return fail(err.Error())
	}
	deps := mint.Deps{
		Garage: garage.Deps{
			HTTP:  httpClient,
			Sleep: garage.Sleep,
			Now:   time.Now,
			Log: func(line string) {
				if _, err := fmt.Fprintln(stdout, line); err != nil && writeErr == nil {
					writeErr = err
				}
			},
		},
		Secrets: secrets,
	}
	if _, err := mint.Run(ctx, cfg, deps); err != nil {
		return fail(err.Error())
	}
	if writeErr != nil {
		return fail("writing mint output: " + writeErr.Error())
	}
	return 0
}

// readConfig reads env in a fixed order and applies the defaults for TTL and renewal.
func readConfig(getenv func(string) string) (mint.Config, error) {
	adminURL, err := jscompat.Required(getenv, "GARAGE_ADMIN_URL")
	if err != nil {
		return mint.Config{}, err
	}
	masterToken, err := jscompat.Required(getenv, "GARAGE_MASTER_TOKEN")
	if err != nil {
		return mint.Config{}, err
	}
	namespace, err := jscompat.Required(getenv, "POD_NAMESPACE")
	if err != nil {
		return mint.Config{}, err
	}
	secretName, err := jscompat.Required(getenv, "SECRET_NAME")
	if err != nil {
		return mint.Config{}, err
	}
	tokenName, err := jscompat.Required(getenv, "TOKEN_NAME")
	if err != nil {
		return mint.Config{}, err
	}
	return mint.Config{
		AdminURL:    garage.TrimTrailingSlashes(adminURL),
		MasterToken: masterToken,
		Namespace:   namespace,
		SecretName:  secretName,
		TokenName:   tokenName,
		Scope:       mint.Scope,
		TTL:         mint.TTL,
		RenewBefore: mint.RenewBefore,
	}, nil
}
