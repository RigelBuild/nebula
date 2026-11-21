package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
	"github.com/RigelBuild/nebula/tools/akiflow/internal/keychain"
)

const (
	callbackPort      = 47811
	loginTimeout      = 5 * time.Minute
	requestedProtocol = "2025-06-18"
	clientName        = "Akiflow TinyCast CLI"
	UsageMessage      = "Usage: akiflow login | akiflow tools | akiflow add <title...>"
)

// endpoints are the remote URLs the CLI calls. Tests point them at an httptest server.
type endpoints struct {
	mcp      string
	token    string
	register string
}

var productionEndpoints = endpoints{ //nolint:gosec // G101: public URL, no credential
	mcp:      "https://mcp.akiflow.com/mcp",
	token:    "https://web.akiflow.com/oauth/token",
	register: "https://web.akiflow.com/oauth/register",
}

// openFunc opens target in the browser and returns the opener's exit code.
type openFunc func(ctx context.Context, target string) (int, error)

// Akiflow holds the dependencies of one CLI run.
type Akiflow struct {
	client    *http.Client
	endpoints endpoints
	now       func() float64
	open      openFunc
	serve     keychain.CallbackServer
	store     keychain.CredentialStore
	nextID    int
}

// NewAkiflow returns a CLI run wired to the production endpoints, clock and browser.
func NewAkiflow(store keychain.CredentialStore) *Akiflow {
	return &Akiflow{
		client:    http.DefaultClient,
		endpoints: productionEndpoints,
		now:       nowMillis,
		open:      keychain.OpenBrowser,
		serve:     keychain.StartCallbackServer,
		store:     store,
	}
}

func nowMillis() float64 {
	return float64(time.Now().UnixMilli())
}

// Dispatch runs one command from argv. Command output goes to stdout; failures are returned.
func Dispatch(ctx context.Context, argv []string, app *Akiflow, stdout io.Writer) error {
	if len(argv) == 0 {
		return errors.New(UsageMessage) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	command, rest := argv[0], argv[1:]
	switch {
	case command == "login" && len(rest) == 0:
		return app.login(ctx, stdout)
	case command == "tools" && len(rest) == 0:
		return app.listTools(ctx, stdout)
	case command == "add":
		title, err := core.TaskTitle(rest)
		if err != nil {
			return err
		}
		return app.add(ctx, stdout, title)
	default:
		return errors.New(UsageMessage) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
}

// WriteLine writes line and a newline to w; the error is the write error.
func WriteLine(w io.Writer, line string) error {
	_, err := io.WriteString(w, line+"\n")
	return err
}
