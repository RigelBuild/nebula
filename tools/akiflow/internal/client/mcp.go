package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
)

const mcpAccept = "application/json, text/event-stream"

// mcpSession is what a tool call needs: the advertised tools and the negotiated session.
type mcpSession struct {
	tools           []core.McpTool
	sessionID       string
	protocolVersion string
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
}

// rpcReply is one successful JSON-RPC reply and the session id to use next.
type rpcReply struct {
	response  core.RPCResponse
	sessionID string
}

// rpc sends one JSON-RPC request. An empty sessionID means the request has no session.
func (a *Akiflow) rpc(ctx context.Context, method string, params any, token, sessionID, protocolVersion string) (rpcReply, error) {
	a.nextID++
	id := a.nextID
	payload, err := json.Marshal(rpcRequest{JSONRPC: core.JSONRPCVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return rpcReply{}, fmt.Errorf("encode mcp request: %w", err)
	}
	reply, err := a.postMCP(ctx, payload, token, sessionID, protocolVersion)
	if err != nil {
		return rpcReply{}, err
	}
	if err := statusError(reply.status, sessionID); err != nil {
		return rpcReply{}, err
	}
	parsed, err := core.ParseRPCResponse(reply.body, reply.contentType, id)
	if err != nil {
		return rpcReply{}, err
	}
	if parsed.HasError {
		return rpcReply{}, rpcFailure(parsed)
	}
	nextSession := reply.header.Get(core.HeaderSessionID)
	if nextSession == "" {
		nextSession = sessionID
	}
	return rpcReply{response: parsed, sessionID: nextSession}, nil
}

func rpcFailure(parsed core.RPCResponse) error {
	if parsed.ErrorMessage != nil {
		return errors.New(*parsed.ErrorMessage)
	}
	return errors.New("MCP request failed")
}

// notifyInitialized sends the notifications/initialized message after the handshake.
func (a *Akiflow) notifyInitialized(ctx context.Context, token, sessionID, protocolVersion string) error {
	payload, err := json.Marshal(notification{JSONRPC: core.JSONRPCVersion, Method: "notifications/initialized"})
	if err != nil {
		return fmt.Errorf("encode mcp notification: %w", err)
	}
	reply, err := a.postMCP(ctx, payload, token, sessionID, protocolVersion)
	if err != nil {
		return err
	}
	return statusError(reply.status, sessionID)
}

// mcpReply is a fully read MCP HTTP reply. The body is already closed.
type mcpReply struct {
	status      int
	header      http.Header
	contentType string
	body        []byte
}

// postMCP sends one MCP HTTP request and reads the whole reply. The body is closed before it returns.
func (a *Akiflow) postMCP(ctx context.Context, payload []byte, token, sessionID, protocolVersion string) (reply mcpReply, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoints.mcp, bytes.NewReader(payload))
	if err != nil {
		return mcpReply{}, fmt.Errorf("build mcp request: %w", err)
	}
	request.Header.Set(core.HeaderAuthorization, "Bearer "+token)
	request.Header.Set(core.HeaderContentType, "application/json")
	request.Header.Set(core.HeaderAccept, mcpAccept)
	if sessionID != "" {
		request.Header.Set(core.HeaderSessionID, sessionID)
	}
	if protocolVersion != "" {
		request.Header.Set(core.HeaderProtocolVersion, protocolVersion)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return mcpReply{}, fmt.Errorf("akiflow mcp request: %w", err)
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return mcpReply{}, fmt.Errorf("read mcp response: %w", err)
	}
	return mcpReply{
		status:      response.StatusCode,
		header:      response.Header,
		contentType: response.Header.Get(core.HeaderContentType),
		body:        body,
	}, nil
}

// statusError returns an httpError for any non-2xx reply.
func statusError(status int, sessionID string) error {
	if status >= http.StatusOK && status <= 299 {
		return nil
	}
	return httpError{status: status, hasSession: sessionID != ""}
}

// initialized runs the MCP handshake and lists the tools.
func (a *Akiflow) initialized(ctx context.Context, token string) (mcpSession, error) {
	handshake, err := a.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": requestedProtocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "akiflow", "version": "0.1.0"},
	}, token, "", "")
	if err != nil {
		return mcpSession{}, err
	}
	protocolVersion := negotiatedProtocol(handshake.response.Result)
	if err := a.notifyInitialized(ctx, token, handshake.sessionID, protocolVersion); err != nil {
		return mcpSession{}, err
	}
	listed, err := a.rpc(ctx, "tools/list", map[string]any{}, token, handshake.sessionID, protocolVersion)
	if err != nil {
		return mcpSession{}, err
	}
	tools, err := parseToolList(listed.response.Result)
	if err != nil {
		return mcpSession{}, err
	}
	return mcpSession{tools: tools, sessionID: listed.sessionID, protocolVersion: protocolVersion}, nil
}

func negotiatedProtocol(result any) string {
	record, ok := core.AsRecord(result)
	if !ok {
		return requestedProtocol
	}
	version, ok := record["protocolVersion"].(string)
	if !ok || version == "" {
		return requestedProtocol
	}
	return version
}

func parseToolList(result any) ([]core.McpTool, error) {
	const invalid = "MCP tools/list returned an invalid result"
	record, ok := core.AsRecord(result)
	if !ok {
		return nil, errors.New(invalid)
	}
	items, ok := record["tools"].([]any)
	if !ok {
		return nil, errors.New(invalid)
	}
	tools := make([]core.McpTool, 0, len(items))
	for _, item := range items {
		entry, ok := core.AsRecord(item)
		if !ok {
			continue
		}
		name, ok := entry["name"].(string)
		if !ok {
			continue
		}
		description, _ := entry["description"].(string)
		tools = append(tools, core.McpTool{Name: name, Description: description, InputSchema: entry["inputSchema"]})
	}
	return tools, nil
}

// withSession runs operation on a fresh session. When the server reports the session expired
// (HTTP 404 on a session request), it starts over once.
func (a *Akiflow) withSession(ctx context.Context, token string, operation func(mcpSession) error) error {
	err := a.runInSession(ctx, token, operation)
	if !isExpiredSession(err) {
		return err
	}
	return a.runInSession(ctx, token, operation)
}

func (a *Akiflow) runInSession(ctx context.Context, token string, operation func(mcpSession) error) error {
	session, err := a.initialized(ctx, token)
	if err != nil {
		return err
	}
	return operation(session)
}

func isExpiredSession(err error) bool {
	replyErr, ok := errors.AsType[httpError](err)
	return ok && replyErr.status == http.StatusNotFound && replyErr.hasSession
}

func (a *Akiflow) listTools(ctx context.Context, stdout io.Writer) error {
	return a.withAuthenticatedToken(ctx, func(token string) error {
		return a.withSession(ctx, token, func(session mcpSession) error {
			for _, tool := range session.tools {
				if err := WriteLine(stdout, toolSummary(tool)); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// toolSummary is name, first description line, and required fields, tab-separated.
func toolSummary(tool core.McpTool) string {
	required := "(none)"
	if fields := core.RequiredFields(tool); len(fields) > 0 {
		required = strings.Join(fields, ", ")
	}
	return fmt.Sprintf("%s\t%s\trequired: %s", tool.Name, firstLine(tool.Description), required)
}

func firstLine(text string) string {
	return core.LineSeparator.Split(text, 2)[0]
}

func (a *Akiflow) add(ctx context.Context, stdout io.Writer, title string) error {
	return a.withAuthenticatedToken(ctx, func(token string) error {
		result, err := a.callAddTool(ctx, token, title)
		if err != nil {
			return err
		}
		if rejected, _ := result["isError"].(bool); rejected {
			return errors.New(toolErrorMessage(result))
		}
		return WriteLine(stdout, "Added: "+title)
	})
}

// callAddTool calls the single task-create tool and returns its result record.
func (a *Akiflow) callAddTool(ctx context.Context, token, title string) (map[string]any, error) {
	var result map[string]any
	err := a.withSession(ctx, token, func(session mcpSession) error {
		tool, err := core.SelectTaskTool(session.tools)
		if err != nil {
			return err
		}
		arguments, err := core.BuildTaskArguments(tool, title)
		if err != nil {
			return err
		}
		reply, err := a.rpc(ctx, "tools/call", map[string]any{"name": tool.Name, "arguments": arguments},
			token, session.sessionID, session.protocolVersion)
		if err != nil {
			return err
		}
		record, ok := core.AsRecord(reply.response.Result)
		if !ok {
			return errors.New("MCP tools/call returned an invalid result")
		}
		result = record
		return nil
	})
	return result, err
}

// toolErrorMessage joins the text items of a failed tool result.
func toolErrorMessage(result map[string]any) string {
	items, _ := result["content"].([]any)
	texts := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := core.AsRecord(item)
		if !ok {
			continue
		}
		if text, ok := entry["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	message := strings.Join(texts, "\n")
	if message == "" {
		return "Tool returned an error"
	}
	return message
}
