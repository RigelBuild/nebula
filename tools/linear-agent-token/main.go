package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/credentialfile"
	"github.com/RigelBuild/nebula/tools/go-lib/linearauth"
	"github.com/spf13/cobra"
)

const (
	// Linear revokes every token of an app when one is minted with a different scope,
	// so this value must never change without rotating the app.
	agentTokenScope = "read,write"
	viewerTimeout   = 10 * time.Second
	graphqlURL      = "https://api.linear.app/graphql"
)

type environment func(string) string

func main() {
	os.Exit(run(context.Background(), os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, getenv environment, stdout, stderr io.Writer) int {
	command := newRootCommand(ctx, getenv, stdout, stderr)
	if err := command.Execute(); err != nil {
		slog.New(slog.NewTextHandler(stderr, nil)).Error("linear-agent-token failed", "error", err)
		return 1
	}
	return 0
}

func newRootCommand(ctx context.Context, getenv environment, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:   "linear-agent-token",
		Short: "Mint the Linear app token",
		Long: "Mints an agent's Linear app-actor token and writes it, plus the app's viewer id,\n" +
			"atomically with mode 0600.\n\n" +
			"Env: LINEAR_CLIENT_ID_FILE, LINEAR_CLIENT_SECRET_FILE, LINEAR_AGENT_TOKEN_FILE,\n" +
			"LINEAR_AGENT_USER_ID_FILE. Exit 0 on success, 1 on any failure.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := mintToFiles(ctx, getenv, http.DefaultClient); err != nil {
				return err
			}
			_, err := fmt.Fprintln(stdout, "linear-agent-token: minted")
			return err
		},
	}
	command.CompletionOptions.DisableDefaultCmd = true
	command.SetOut(stdout)
	command.SetErr(stderr)
	return command
}

func mintToFiles(ctx context.Context, getenv environment, client *http.Client) error {
	tokenFile := strings.TrimSpace(getenv("LINEAR_AGENT_TOKEN_FILE"))
	userIDFile := strings.TrimSpace(getenv("LINEAR_AGENT_USER_ID_FILE"))
	if tokenFile == "" || userIDFile == "" {
		return errors.New("LINEAR_AGENT_TOKEN_FILE and LINEAR_AGENT_USER_ID_FILE must be set")
	}
	clientID, hasClientID := credentialfile.Read(getenv("LINEAR_CLIENT_ID_FILE"))
	clientSecret, hasClientSecret := credentialfile.Read(getenv("LINEAR_CLIENT_SECRET_FILE"))
	if !hasClientID || !hasClientSecret {
		return errors.New("linear agent app client credentials are missing")
	}
	credentials := linearauth.Credentials{ClientID: clientID, ClientSecret: clientSecret}
	token, err := linearauth.MintAppToken(ctx, client, credentials, agentTokenScope)
	if err != nil {
		return fmt.Errorf("minting app token: %w", err)
	}
	userID, err := viewerID(ctx, client, token.AccessToken)
	if err != nil {
		return err
	}
	if err := writeAtomic(tokenFile, token.AccessToken); err != nil {
		return fmt.Errorf("writing agent token: %w", err)
	}
	if err := writeAtomic(userIDFile, userID); err != nil {
		return fmt.Errorf("writing agent user id: %w", err)
	}
	return nil
}

// viewerID returns the app actor's id; the comment poller drops comments by it.
func viewerID(ctx context.Context, client *http.Client, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, viewerTimeout)
	defer cancel()
	body, err := json.Marshal(struct {
		Query string `json:"query"`
	}{Query: "query { viewer { id } }"})
	if err != nil {
		return "", fmt.Errorf("encoding Linear viewer query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating Linear viewer request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", token)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("sending Linear viewer request: %w", err)
	}
	responseBody, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", fmt.Errorf("reading Linear viewer response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("viewer query failed: %d", response.StatusCode)
	}
	var payload struct {
		Data struct {
			Viewer struct {
				ID string `json:"id"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return "", fmt.Errorf("decoding Linear viewer response: %w", err)
	}
	if payload.Data.Viewer.ID == "" {
		return "", errors.New("viewer query returned no id")
	}
	return payload.Data.Viewer.ID, nil
}

func writeAtomic(path, value string) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The path comes from the operator's env, beside the destination.
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	defer func() {
		if returnErr != nil {
			removeErr := os.Remove(temporary)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("removing temporary file: %w", removeErr))
			}
		}
	}()
	if _, err := io.WriteString(file, value+"\n"); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("writing temporary file: %w", err), closeErr)
	}
	if err := file.Chmod(0o600); err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("setting temporary file mode: %w", err), closeErr)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replacing destination: %w", err)
	}
	return nil
}
