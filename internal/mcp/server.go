// Package mcp is a minimal Model Context Protocol server over the Streamable
// HTTP transport. It exposes a registry of tools that a Claude session can call.
// It implements the small subset the orchestrator needs: initialize,
// tools/list, tools/call, and ping. Tool calls are scoped to a session id taken
// from the request path, so each manager's Claude gets a server bound to its own
// session.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
)

// defaultProtocolVersion is echoed when a client does not specify one.
const defaultProtocolVersion = "2025-06-18"

// ToolHandler executes a tool call. The context carries the calling session id
// (see SessionFromContext). It returns human/agent-readable text.
type ToolHandler func(ctx context.Context, args map[string]any) (string, error)

// Tool is a registered tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema for arguments
	Handler     ToolHandler
}

// Server is an MCP tool server.
type Server struct {
	name    string
	version string
	tools   []Tool
	byName  map[string]Tool
}

// NewServer creates a server with the given implementation name/version.
func NewServer(name, version string) *Server {
	return &Server{name: name, version: version, byName: map[string]Tool{}}
}

// AddTool registers a tool.
func (s *Server) AddTool(t Tool) {
	s.tools = append(s.tools, t)
	s.byName[t.Name] = t
}

type ctxKey string

const sessionKey ctxKey = "mcp.session"

// SessionFromContext returns the session id bound to the request, if any.
func SessionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionKey).(string)
	return v
}

// WithSession binds a session id to a context (exported for tests).
func WithSession(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, sessionKey, session)
}

// Handler returns an http.Handler. It serves POST /{session} (and bare POST /)
// as the MCP endpoint; mount it under a prefix such as "/mcp/".
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{session}", s.serve)
	mux.HandleFunc("POST /", s.serve)
	return mux
}

// ---- JSON-RPC types ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	session := r.PathValue("session")
	ctx := WithSession(r.Context(), session)

	// Notifications (no id) get a 202 with no body.
	isNotification := len(req.ID) == 0

	resp := s.dispatch(ctx, req)
	if isNotification {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, resp)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) rpcResponse {
	base := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		base.Result = s.initialize(req.Params)
	case "notifications/initialized", "notifications/cancelled":
		// no-op notifications
	case "ping":
		base.Result = map[string]any{}
	case "tools/list":
		base.Result = map[string]any{"tools": s.toolList()}
	case "tools/call":
		res, errResult := s.callTool(ctx, req.Params)
		base.Result = res
		if errResult != nil {
			base.Result = errResult
		}
	default:
		base.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return base
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	proto := defaultProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		proto = p.ProtocolVersion // echo the client's version for compatibility
	}
	return map[string]any{
		"protocolVersion": proto,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": s.name, "version": s.version},
	}
}

func (s *Server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
		})
	}
	return out
}

// callTool runs a tool and returns an MCP tool result. Tool execution errors are
// reported inside the result with isError=true (per MCP), not as JSON-RPC errors.
func (s *Server) callTool(ctx context.Context, params json.RawMessage) (result map[string]any, _ map[string]any) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResult("invalid tool call params: " + err.Error()), nil
	}
	t, ok := s.byName[p.Name]
	if !ok {
		return errorResult("unknown tool: " + p.Name), nil
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	out, err := t.Handler(ctx, p.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": out}},
		"isError": false,
	}, nil
}

func errorResult(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- argument helpers for tool handlers ----

// StringArg returns a string argument (empty if missing/!string).
func StringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// BoolArg returns a bool argument (false if missing).
func BoolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// StringsArg returns a []string argument, tolerating []any of strings.
func StringsArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
