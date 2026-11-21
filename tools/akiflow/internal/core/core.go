package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const (
	authorizeURL             = "https://web.akiflow.com/oauth/authorize"
	invalidStoredCredentials = "Invalid stored Akiflow credentials; run: akiflow login"
	defaultExpiresInSeconds  = 3600

	JSONRPCVersion        = "2.0"
	HeaderAuthorization   = "Authorization"
	HeaderContentType     = "Content-Type"
	HeaderAccept          = "Accept"
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderProtocolVersion = "Mcp-Protocol-Version"
	GrantAuthCode         = "authorization_code"
	GrantRefreshToken     = "refresh_token"
	ParamClientID         = "client_id"
	ParamCode             = "code"
)

// Tokens is the credential record kept in the Keychain. Field order matches the stored JSON.
type Tokens struct {
	ClientID     string  `json:"client_id"`
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresAt    float64 `json:"expires_at"`
}

// McpTool is one tool advertised by the Akiflow MCP server.
type McpTool struct {
	Name        string
	Description string
	InputSchema any
}

// RPCResponse is one JSON-RPC reply for a request id.
// HasError is set when the reply carries an error object; ErrorMessage is nil when that object has no string message.
type RPCResponse struct {
	Result       any
	HasError     bool
	ErrorMessage *string
}

// AsRecord returns value as a JSON object, and reports whether it was one.
func AsRecord(value any) (map[string]any, bool) {
	record, ok := value.(map[string]any)
	return record, ok
}

func RequiredString(value any, name string) (string, error) {
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("Invalid %s in server response", name) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return text, nil
}

func MakePkceVerifier(randomBytes []byte) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes)
}

func MakePkceChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// MakeState encodes random bytes as the URL-safe OAuth state value.
func MakeState(randomBytes []byte) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes)
}

// FormEncode serializes pairs in order, as URLSearchParams.toString does.
func FormEncode(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, formEscape(pair[0])+"="+formEscape(pair[1]))
	}
	return strings.Join(parts, "&")
}

// formEscape matches URLSearchParams: alphanumerics and *-._ stay, space becomes +, the rest is %XX.
func formEscape(text string) string {
	var out strings.Builder
	for i := range len(text) {
		c := text[i]
		switch {
		case c == ' ':
			out.WriteByte('+')
		case isFormSafe(c):
			out.WriteByte(c)
		default:
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

func isFormSafe(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '*' || c == '-' || c == '.' || c == '_'
}

func BuildAuthorizationURL(clientID, redirectURI, state, codeChallenge string) string {
	return authorizeURL + "?" + FormEncode([][2]string{
		{"response_type", ParamCode},
		{ParamClientID, clientID},
		{"redirect_uri", redirectURI},
		{"scope", "mcp:read mcp:write"},
		{"state", state},
		{"code_challenge", codeChallenge},
		{"code_challenge_method", "S256"},
	})
}

// CallbackCode returns the authorization code from a callback URL after checking state.
func CallbackCode(callback *url.URL, expectedState string) (string, error) {
	query := callback.Query()
	if query.Get("state") != expectedState {
		return "", errors.New("OAuth state mismatch")
	}
	if callbackError := query.Get("error"); callbackError != "" {
		return "", fmt.Errorf("OAuth authorization failed: %s", callbackError)
	}
	return RequiredString(query.Get("code"), "authorization code")
}

var taskToolName = regexp.MustCompile(`(?i)^(add|create)[-_]?tasks?$`)

// SelectTaskTool returns the single tool whose name is an add or create task name.
func SelectTaskTool(tools []McpTool) (McpTool, error) {
	var matches []McpTool
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
		if taskToolName.MatchString(tool.Name) {
			matches = append(matches, tool)
		}
	}
	if len(matches) != 1 {
		available := strings.Join(names, ", ")
		if available == "" {
			available = "(none)"
		}
		return McpTool{}, fmt.Errorf("Expected one task-create tool; found %d. Available tools: %s", len(matches), available) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return matches[0], nil
}

// RequiredFields lists the string entries of the schema's required array.
func RequiredFields(tool McpTool) []string {
	schema, ok := AsRecord(tool.InputSchema)
	if !ok {
		return nil
	}
	list, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	fields := make([]string, 0, len(list))
	for _, item := range list {
		if name, ok := item.(string); ok {
			fields = append(fields, name)
		}
	}
	return fields
}

// BuildTaskArguments sends only the title; any other required field is an error.
func BuildTaskArguments(tool McpTool, title string) (map[string]any, error) {
	schema, ok := AsRecord(tool.InputSchema)
	if !ok {
		return nil, fmt.Errorf("%s has no usable input schema", tool.Name)
	}
	properties, ok := AsRecord(schema["properties"])
	if !ok {
		return nil, fmt.Errorf("%s has no usable input schema", tool.Name)
	}
	if _, present := properties["title"]; !present {
		return nil, fmt.Errorf("%s input schema has no title property", tool.Name)
	}
	var extras []string
	for _, field := range RequiredFields(tool) {
		if field != "title" {
			extras = append(extras, field)
		}
	}
	if len(extras) > 0 {
		return nil, fmt.Errorf("%s requires unsupported fields: %s", tool.Name, strings.Join(extras, ", "))
	}
	return map[string]any{"title": title}, nil
}

func hasID(record map[string]any, id int) bool {
	value, ok := record["id"].(float64)
	return ok && value == float64(id)
}

func parseRpcValue(value any, id int) (RPCResponse, error) {
	record, ok := AsRecord(value)
	if !ok || !hasID(record, id) {
		return RPCResponse{}, fmt.Errorf("Invalid MCP JSON-RPC response %d", id) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	if rpcVersion, present := record["jsonrpc"]; present && rpcVersion != JSONRPCVersion {
		return RPCResponse{}, fmt.Errorf("Invalid MCP JSON-RPC response %d", id) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	response := RPCResponse{Result: record["result"]}
	if rawError, present := record["error"]; present {
		errorRecord, ok := AsRecord(rawError)
		if !ok {
			return RPCResponse{}, fmt.Errorf("Invalid MCP JSON-RPC error %d", id) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
		}
		response.HasError = true
		if message, ok := errorRecord["message"].(string); ok {
			response.ErrorMessage = &message
		}
	}
	return response, nil
}

var (
	eventSeparator = regexp.MustCompile(`\r?\n\r?\n`)
	LineSeparator  = regexp.MustCompile(`\r?\n`)
)

// parseSseResponse returns the reply with the matching id from the first event that carries it.
func parseSseResponse(body string, id int) (RPCResponse, error) {
	for _, event := range eventSeparator.Split(body, -1) {
		data := eventData(event)
		if data == "" || data == "[DONE]" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			continue
		}
		if record, ok := AsRecord(value); ok && hasID(record, id) {
			return parseRpcValue(value, id)
		}
	}
	return RPCResponse{}, fmt.Errorf("MCP response %d was not present in the event stream", id)
}

// eventData joins the data: lines of one SSE event.
func eventData(event string) string {
	rawLines := LineSeparator.Split(event, -1)
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			lines = append(lines, strings.TrimLeftFunc(payload, unicode.IsSpace))
		}
	}
	return strings.Join(lines, "\n")
}

// ParseRPCResponse reads an MCP reply as an SSE stream or one JSON body, chosen by content type.
func ParseRPCResponse(body []byte, contentType string, id int) (RPCResponse, error) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSseResponse(string(body), id)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return RPCResponse{}, fmt.Errorf("decode mcp response: %w", err)
	}
	return parseRpcValue(value, id)
}

// ParseTokens validates a token response. expires_at is now plus expires_in, default 3600 seconds.
func ParseTokens(value any, now float64) (Tokens, error) {
	record, ok := AsRecord(value)
	if !ok {
		return Tokens{}, errors.New("Invalid token response") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	clientID, err := RequiredString(record["client_id"], "client_id")
	if err != nil {
		return Tokens{}, err
	}
	access, err := RequiredString(record["access_token"], "access_token")
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := RequiredString(record["refresh_token"], "refresh_token")
	if err != nil {
		return Tokens{}, err
	}
	expiresIn := float64(defaultExpiresInSeconds)
	if seconds, ok := record["expires_in"].(float64); ok {
		expiresIn = seconds
	}
	return Tokens{
		ClientID:     clientID,
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    now + expiresIn*1000,
	}, nil
}

// ParseStoredTokens decodes the JSON credential record read from the Keychain.
func ParseStoredTokens(raw string) (Tokens, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return Tokens{}, errors.New(invalidStoredCredentials) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	record, ok := AsRecord(value)
	if !ok {
		return Tokens{}, errors.New(invalidStoredCredentials) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	clientID, okID := record["client_id"].(string)
	access, okAccess := record["access_token"].(string)
	refresh, okRefresh := record["refresh_token"].(string)
	expires, okExpires := record["expires_at"].(float64)
	if !okID || !okAccess || !okRefresh || !okExpires {
		return Tokens{}, errors.New(invalidStoredCredentials) //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return Tokens{ClientID: clientID, AccessToken: access, RefreshToken: refresh, ExpiresAt: expires}, nil
}

// TaskTitle joins the add arguments into one title, and rejects a blank one.
func TaskTitle(args []string) (string, error) {
	title := strings.Join(args, " ")
	if strings.TrimSpace(title) == "" {
		return "", errors.New("Task title must not be empty") //nolint:staticcheck // ST1005: user-facing message, text is fixed.
	}
	return title, nil
}
