package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tichopad/bobik/internal/fireworks"
)

func TestDirectMCPCallAndAudit(t *testing.T) {
	cfg := config(t)
	closed := false
	tool := MCPTool{
		Server: "local", Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`),
		Call: func(_ context.Context, args json.RawMessage) (string, error) {
			if string(args) != `{"query":"private query"}` {
				t.Fatalf("arguments: %s", args)
			}
			return "private snippet", nil
		},
	}
	cfg.OpenMCP = func(context.Context) ([]MCPTool, func()) {
		return []MCPTool{tool}, func() { closed = true }
	}
	cfg.ModelClient = completeFunc(func(_ context.Context, req fireworks.Request) (fireworks.Response, error) {
		name := "mcp_local__search"
		if req.Tools[len(req.Tools)-1].Name != name {
			t.Fatalf("missing direct tool: %v", req.Tools)
		}
		if len(req.Messages) == 2 {
			return fireworks.Response{FinishReason: "tool_calls", Message: fireworks.Message{
				ToolCalls: []fireworks.Call{{ID: "search-1", Name: name, Arguments: `{"query":"private query"}`}},
			}}, nil
		}
		if m := req.Messages[len(req.Messages)-1]; m.ToolCallID != "search-1" || m.Content != "private snippet" {
			t.Fatalf("wrong result: %+v", m)
		}
		return fireworks.Response{FinishReason: "stop", Message: fireworks.Message{Content: "Done."}}, nil
	})
	summary, err := Run(t.Context(), cfg)
	if err != nil || !closed {
		t.Fatalf("closed=%t, err=%v", closed, err)
	}
	log, err := os.ReadFile(summary.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "private query") || strings.Contains(string(log), "private snippet") ||
		!strings.Contains(string(log), `"server":"local"`) || !strings.Contains(string(log), `"outcome":"success"`) {
		t.Fatalf("unexpected audit log: %s", log)
	}
}

func TestModelToolNames(t *testing.T) {
	seen := make(map[string]bool)
	for _, pair := range [][2]string{
		{"a", "b__c"}, {"a__b", "c"}, {"a_b", "c"}, {"a_", "b"}, {"a", "_b"},
		{"search", "find.v2"}, {"search", "find_v2"}, {"long", strings.Repeat("z", 80)},
	} {
		name := modelToolName(pair[0], pair[1])
		if seen[name] || len(name) > 64 || unsafeName.MatchString(name) {
			t.Fatalf("invalid or duplicate tool name %q", name)
		}
		seen[name] = true
	}
}
