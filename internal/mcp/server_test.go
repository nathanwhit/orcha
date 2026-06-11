package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer() *Server {
	s := NewServer("test", "0.1")
	s.AddTool(Tool{
		Name:        "echo",
		Description: "echoes its message, scoped to the session",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}, "required": []string{"message"}},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			return SessionFromContext(ctx) + ":" + StringArg(args, "message"), nil
		},
	})
	return s
}

func post(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestInitialize_EchoesProtocol(t *testing.T) {
	h := newTestServer().Handler()
	_, resp := post(t, h, "/sess1", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	res := resp["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion=%v", res["protocolVersion"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("expected tools capability")
	}
}

func TestNotificationGets202(t *testing.T) {
	h := newTestServer().Handler()
	code, _ := post(t, h, "/sess1", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Fatalf("notification status=%d, want 202", code)
	}
}

func TestToolsList(t *testing.T) {
	h := newTestServer().Handler()
	_, resp := post(t, h, "/sess1", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list = %+v", tools)
	}
}

func TestToolsCall_ScopedToSession(t *testing.T) {
	h := newTestServer().Handler()
	_, resp := post(t, h, "/sessABC",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hi"}}}`)
	res := resp["result"].(map[string]any)
	if res["isError"].(bool) {
		t.Fatalf("unexpected error: %+v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"]
	if text != "sessABC:hi" {
		t.Fatalf("tool result=%v, want sessABC:hi (session scoping)", text)
	}
}

func TestToolsCall_UnknownToolIsErrorResult(t *testing.T) {
	h := newTestServer().Handler()
	_, resp := post(t, h, "/s", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	res := resp["result"].(map[string]any)
	if !res["isError"].(bool) {
		t.Fatal("unknown tool should be an isError result, not a hard failure")
	}
}

func TestUnknownMethodIsJSONRPCError(t *testing.T) {
	h := newTestServer().Handler()
	_, resp := post(t, h, "/s", `{"jsonrpc":"2.0","id":5,"method":"bogus"}`)
	if resp["error"] == nil {
		t.Fatal("unknown method should return a JSON-RPC error")
	}
}
