package core

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func mcpTool(properties map[string]any, required []any) McpTool {
	const name = "add-task"
	return McpTool{
		Name:        name,
		InputSchema: map[string]any{"type": "object", "properties": properties, "required": required},
	}
}

func titleProperty() map[string]any {
	return map[string]any{"title": map[string]any{"type": "string"}}
}

func TestTaskTitle(t *testing.T) {
	title, err := TaskTitle([]string{"buy", "milk"})
	if err != nil || title != "buy milk" {
		t.Fatalf("TaskTitle(buy milk) = %q, %v; want %q", title, err, "buy milk")
	}
	_, err = TaskTitle([]string{})
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("TaskTitle(empty) error = %v; want one containing %q", err, "must not be empty")
	}
}

func TestSelectTaskTool(t *testing.T) {
	t.Run("selects the single matching tool", func(t *testing.T) {
		tool, err := SelectTaskTool([]McpTool{
			{Name: "search"},
			mcpTool(titleProperty(), []any{"title"}),
		})
		if err != nil || tool.Name != "add-task" {
			t.Fatalf("SelectTaskTool = %q, %v; want add-task", tool.Name, err)
		}
	})
	t.Run("rejects no match and lists available tools", func(t *testing.T) {
		_, err := SelectTaskTool([]McpTool{{Name: "search"}})
		if err == nil || !strings.Contains(err.Error(), "search") {
			t.Fatalf("SelectTaskTool error = %v; want one naming search", err)
		}
	})
	t.Run("rejects multiple matches and lists their names", func(t *testing.T) {
		_, err := SelectTaskTool([]McpTool{{Name: "add-task"}, {Name: "create_task"}})
		if err == nil || !strings.Contains(err.Error(), "add-task, create_task") {
			t.Fatalf("SelectTaskTool error = %v; want one naming add-task, create_task", err)
		}
	})
}

func TestBuildTaskArguments(t *testing.T) {
	t.Run("sends only the title", func(t *testing.T) {
		args, err := BuildTaskArguments(mcpTool(titleProperty(), []any{"title"}), "buy milk")
		if err != nil || !reflect.DeepEqual(args, map[string]any{"title": "buy milk"}) {
			t.Fatalf("BuildTaskArguments = %v, %v; want only title", args, err)
		}
	})
	t.Run("rejects a schema without title", func(t *testing.T) {
		properties := map[string]any{"content": map[string]any{"type": "string"}}
		_, err := BuildTaskArguments(mcpTool(properties, []any{"title"}), "buy milk")
		if err == nil || !strings.Contains(err.Error(), "no title") {
			t.Fatalf("BuildTaskArguments error = %v; want one naming no title", err)
		}
	})
	t.Run("rejects additional required fields", func(t *testing.T) {
		properties := map[string]any{"title": map[string]any{}, "date": map[string]any{}}
		_, err := BuildTaskArguments(mcpTool(properties, []any{"title", "date"}), "buy milk")
		if err == nil || !strings.Contains(err.Error(), "date") {
			t.Fatalf("BuildTaskArguments error = %v; want one naming date", err)
		}
	})
}

func TestMCPResponseParsing(t *testing.T) {
	t.Run("parses JSON response bodies", func(t *testing.T) {
		body := []byte(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`)
		parsed, err := ParseRPCResponse(body, "application/json", 7)
		if err != nil || !reflect.DeepEqual(parsed.Result, map[string]any{"ok": true}) {
			t.Fatalf("ParseRPCResponse = %v, %v; want result {ok:true}", parsed.Result, err)
		}
	})
	t.Run("selects the matching response from a later SSE event", func(t *testing.T) {
		stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n" +
			"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n\n"
		parsed, err := parseSseResponse(stream, 7)
		if err != nil || !reflect.DeepEqual(parsed.Result, map[string]any{"ok": true}) {
			t.Fatalf("parseSseResponse = %v, %v; want result {ok:true}", parsed.Result, err)
		}
	})
}

func TestOAuthAuthorizationURL(t *testing.T) {
	t.Run("builds an authorization URL with PKCE fields", func(t *testing.T) {
		raw := BuildAuthorizationURL("client", "http://127.0.0.1/callback", "s", "c")
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse authorization URL: %v", err)
		}
		if got := parsed.Query().Get("code_challenge_method"); got != "S256" {
			t.Errorf("code_challenge_method = %q; want S256", got)
		}
		if got := parsed.Query().Get("state"); got != "s" {
			t.Errorf("state = %q; want s", got)
		}
	})
}
