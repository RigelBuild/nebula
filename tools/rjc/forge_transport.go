package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/samber/oops"
)

const forgeMessageLimit = 200

type forgeAPIError struct {
	status  int
	message string
	cause   error
}

func (e *forgeAPIError) Error() string {
	return fmt.Sprintf("forge request failed (HTTP %d): %s", e.status, e.message)
}
func (e *forgeAPIError) Unwrap() error { return e.cause }

func forgeFailure(status int, body []byte, cause error) error {
	var response struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &response); err == nil && response.Message != "" {
		return &forgeAPIError{status: status, message: response.Message, cause: cause}
	}

	message := strings.ToValidUTF8(strings.TrimSpace(string(body)), "\uFFFD")
	if message == "" {
		message = http.StatusText(status)
		if message == "" {
			message = "no response body"
		}
	}
	if len(message) > forgeMessageLimit {
		cut := forgeMessageLimit - len("…")
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut] + "…"
	}
	return &forgeAPIError{status: status, message: message, cause: cause}
}

func (f *forgeClient) ghAPI(ctx context.Context, args []string, input []byte) ([]byte, error) {
	output, commandErr := f.repo.command(ctx, []string{"GH_TOKEN=" + f.token}, input, "gh", args...)
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(output)), nil)
	if err != nil {
		return nil, oops.Wrapf(errors.Join(commandErr, err), "read GitHub HTTP response")
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, oops.Wrapf(errors.Join(commandErr, err), "read GitHub response body")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, forgeFailure(response.StatusCode, body, commandErr)
	}
	if commandErr != nil {
		return nil, commandErr
	}
	return body, nil
}
