package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/client"
	"github.com/RigelBuild/nebula/tools/akiflow/internal/keychain"
)

const (
	keychainServiceEnv = "AKIFLOW_KEYCHAIN_SERVICE"
	keychainAccountEnv = "AKIFLOW_KEYCHAIN_ACCOUNT"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run builds the production dependencies, dispatches argv, and returns the exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		if err := client.WriteLine(stdout, client.UsageMessage); err != nil {
			return reportFailure(stderr, err)
		}
		return 0
	}
	service := getenv(keychainServiceEnv)
	if service == "" {
		return reportFailure(stderr, errors.New("Keychain service is required; set AKIFLOW_KEYCHAIN_SERVICE")) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	account := getenv(keychainAccountEnv)
	if account == "" {
		return reportFailure(stderr, errors.New("Keychain account is required; set AKIFLOW_KEYCHAIN_ACCOUNT")) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	store, err := keychain.NewKeychainStore(service, account, keychain.ExecRunner)
	if err != nil {
		return reportFailure(stderr, err)
	}
	if dispatchErr := client.Dispatch(ctx, args, client.NewAkiflow(store), stdout); dispatchErr != nil {
		return reportFailure(stderr, dispatchErr)
	}
	return 0
}

// reportFailure writes the bare error message and a newline to stderr, then returns exit code 1, as the TS runCli did.
func reportFailure(stderr io.Writer, err error) int {
	// The exit code is already 1 and stderr is the only channel left, so a failed write is not actionable here.
	_ = client.WriteLine(stderr, err.Error())
	return 1
}
