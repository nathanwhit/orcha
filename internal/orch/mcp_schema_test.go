package orch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestToolsList_RequiredIsNeverNull guards a bug that silently bricked every
// manager: a tool built with no required fields serialized "required" as JSON
// null (a nil variadic slice), and Claude Code's strict Zod validation rejects
// the ENTIRE tools/list when any tool has `"required": null` — leaving the agent
// with zero orcha tools and no way to recover. JSON Schema's "required" must be
// an array; assert every tool on every surface emits one.
func TestToolsList_RequiredIsNeverNull(t *testing.T) {
	o, _ := newTestOrch(t)
	handlers := map[string]http.Handler{
		"manager":  o.ManagerMCPHandler(),
		"worker":   o.WorkerMCPHandler(),
		"followup": o.FollowupMCPHandler(),
		"operator": o.OperatorMCPHandler(),
	}
	for surface, h := range handlers {
		req := httptest.NewRequest("POST", "/sess1", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		var resp struct {
			Result struct {
				Tools []struct {
					Name        string `json:"name"`
					InputSchema struct {
						Required *json.RawMessage `json:"required"`
					} `json:"inputSchema"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: unmarshal tools/list: %v", surface, err)
		}
		if len(resp.Result.Tools) == 0 {
			t.Fatalf("%s: no tools returned", surface)
		}
		for _, tool := range resp.Result.Tools {
			r := tool.InputSchema.Required
			// Present-and-null is the bug. Absent (omitted) would be fine, but our
			// builder always sets it, so require a JSON array.
			if r == nil || string(*r) == "null" {
				t.Errorf("%s tool %q: inputSchema.required is null, want a JSON array", surface, tool.Name)
				continue
			}
			var arr []string
			if err := json.Unmarshal(*r, &arr); err != nil {
				t.Errorf("%s tool %q: inputSchema.required is not an array: %s", surface, tool.Name, string(*r))
			}
		}
	}
}
