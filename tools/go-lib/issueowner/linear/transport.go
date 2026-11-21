// SPDX-License-Identifier: MIT OR Apache-2.0

package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/credentialfile"
	"github.com/RigelBuild/nebula/tools/go-lib/issueowner"
	"github.com/RigelBuild/nebula/tools/go-lib/linearauth"
)

// RateLimitError describes a Linear rate-limit response and its reset metadata.
type RateLimitError struct {
	RetryAfter, RequestsResetAt, ComplexityResetAt *float64
	StatusCode                                     int
	Message                                        string
}

func (e *RateLimitError) Error() string {
	if e.Message != "" {
		return "Linear rate limited: " + e.Message
	}
	return "Linear rate limited"
}

// HTTPError describes a non-rate-limited non-success HTTP response.
type HTTPError struct {
	StatusCode   int
	Status, Body string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("Linear GraphQL HTTP request failed: %d %s", e.StatusCode, e.Status)
	}
	return fmt.Sprintf("Linear GraphQL HTTP request failed: %d %s: %s", e.StatusCode, e.Status, e.Body)
}

// GraphQLError carries messages returned by a successful GraphQL HTTP response.
type GraphQLError struct{ Messages []string }

func (e *GraphQLError) Error() string {
	return "Linear GraphQL request failed: " + strings.Join(e.Messages, "; ")
}

// IssueClient is the hand-written Linear GraphQL transport.
type IssueClient struct {
	httpClient              *http.Client
	endpoint, authorization string
}

// NewIssueAPI selects credentials and creates a GraphQL client.
func NewIssueAPI(ctx context.Context, httpClient *http.Client, endpoint string, getenv func(string) string) (issueowner.LinearIssueAPI, error) {
	if ctx == nil {
		return nil, errors.New("creating Linear API: nil context")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if endpoint == "" {
		endpoint = linearGraphQLEndpoint
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	clientID, clientSecret := getenv("LINEAR_CLIENT_ID"), getenv("LINEAR_CLIENT_SECRET")
	if (clientID == "") != (clientSecret == "") {
		return nil, errors.New("partial app-actor credential: LINEAR_CLIENT_ID and LINEAR_CLIENT_SECRET must be set together (one is set, the other is empty or missing). Set both, or unset both to use LINEAR_API_KEY")
	}
	var authorization string
	if clientID != "" {
		token, err := linearauth.MintAppToken(ctx, httpClient, linearauth.Credentials{ClientID: clientID, ClientSecret: clientSecret}, "read,write")
		if err != nil {
			return nil, fmt.Errorf("minting Linear app token: %w", err)
		}
		authorization = "Bearer " + token.AccessToken
	} else {
		apiKey := ReadAPIKey(getenv)
		if apiKey == "" {
			return nil, errors.New("no Linear credential: set LINEAR_CLIENT_ID + LINEAR_CLIENT_SECRET (app-actor grant) or LINEAR_API_KEY")
		}
		authorization = apiKey
	}
	return &IssueClient{httpClient: withoutRedirects(httpClient), endpoint: endpoint, authorization: authorization}, nil
}

// NewIssueAPIWithToken creates a GraphQL client using a bearer access token.
func NewIssueAPIWithToken(httpClient *http.Client, endpoint, accessToken string) issueowner.LinearIssueAPI {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if endpoint == "" {
		endpoint = linearGraphQLEndpoint
	}
	return &IssueClient{
		httpClient:    withoutRedirects(httpClient),
		endpoint:      endpoint,
		authorization: "Bearer " + accessToken,
	}
}

// withoutRedirects returns a shallow copy of client that never follows a
// redirect, so the credential is never sent to a redirect target. The
// caller's client is left unchanged.
func withoutRedirects(client *http.Client) *http.Client {
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copied
}

// ReadAPIKey reads a nonempty key file first, then LINEAR_API_KEY.
func ReadAPIKey(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	if value, ok := credentialfile.Read(getenv("LINEAR_API_KEY_FILE")); ok {
		return value
	}
	return getenv("LINEAR_API_KEY")
}

type graphQLRequestBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}
type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Type string `json:"type"`
		} `json:"extensions"`
	} `json:"errors"`
}

func firstErrorIsRateLimited(graphErrors []struct {
	Message    string `json:"message"`
	Extensions struct {
		Type string `json:"type"`
	} `json:"extensions"`
}) bool {
	return len(graphErrors) > 0 && graphErrors[0].Extensions.Type == "ratelimited"
}

func parseOptionalNumber(value string) *float64 {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}
func (c *IssueClient) request(ctx context.Context, query string, variables map[string]any, output any) error {
	if ctx == nil {
		return errors.New("sending Linear GraphQL request: nil context")
	}
	body, err := json.Marshal(graphQLRequestBody{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("encoding Linear GraphQL request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating Linear GraphQL request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", c.authorization)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("sending Linear GraphQL request: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			_ = err
		}
	}()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("reading Linear GraphQL response: %w", err)
	}
	return decodeResponse(response, responseBody, output)
}

func decodeResponse(response *http.Response, responseBody []byte, output any) error {
	var decoded graphQLResponse
	decodeErr := json.Unmarshal(responseBody, &decoded)
	rateLimited := len(decoded.Errors) == 0 && response.StatusCode == http.StatusTooManyRequests
	if len(decoded.Errors) > 0 {
		rateLimited = firstErrorIsRateLimited(decoded.Errors)
	}
	if rateLimited {
		message := ""
		if decodeErr == nil && len(decoded.Errors) > 0 {
			message = decoded.Errors[0].Message
		}
		return &RateLimitError{RetryAfter: parseOptionalNumber(response.Header.Get("Retry-After")), RequestsResetAt: parseOptionalNumber(response.Header.Get("X-Ratelimit-Requests-Reset")), ComplexityResetAt: parseOptionalNumber(response.Header.Get("X-Ratelimit-Complexity-Reset")), StatusCode: response.StatusCode, Message: message}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &HTTPError{StatusCode: response.StatusCode, Status: response.Status, Body: string(responseBody)}
	}
	if decodeErr != nil {
		return fmt.Errorf("decoding Linear GraphQL response: %w", decodeErr)
	}
	if len(decoded.Errors) > 0 {
		messages := make([]string, 0, len(decoded.Errors))
		for _, graphQLError := range decoded.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return &GraphQLError{Messages: messages}
	}
	if err := json.Unmarshal(decoded.Data, output); err != nil {
		return fmt.Errorf("decoding Linear GraphQL data: %w", err)
	}
	return nil
}
