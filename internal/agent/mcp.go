package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tichopad/bobik/internal/fireworks"
)

// MCPTool is a discovered tool bound to its server for the current run.
type MCPTool struct {
	Server, Name, Description string
	InputSchema               json.RawMessage
	Call                      func(context.Context, json.RawMessage) (string, error)
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func modelToolName(server, tool string) string {
	name := "mcp_" + server + "__" + tool
	if len(name) <= 64 && !unsafeName.MatchString(name) && !strings.Contains(server, "__") && !strings.HasSuffix(server, "_") && !strings.Contains(tool, "__") && !strings.HasPrefix(tool, "_") {
		return name
	}
	// Reserve a separate prefix for shortened names.
	short := "mcp-h-" + unsafeName.ReplaceAllString(server+"_"+tool, "_")
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	return short[:min(len(short), 43)] + "-" + fmt.Sprintf("%x", sum[:10])
}

func (s *runState) runMCPTool(ctx context.Context, step int, call fireworks.Call, tool MCPTool) (fireworks.Message, error) {
	event := map[string]any{"step": step, "toolCallId": call.ID, "toolName": tool.Name, "server": tool.Server}
	if err := s.log.Event("tool-call", event); err != nil {
		return fireworks.Message{}, err
	}
	if s.cfg.Verbose != nil {
		fmt.Fprintf(s.cfg.Verbose, "bobik: step %d MCP tool %q/%q\n", step, tool.Server, tool.Name)
	}
	start := time.Now()
	args := strings.TrimSpace(call.Arguments)
	var content string
	var err error
	switch {
	case len(call.Arguments) > maxArguments:
		err = fmt.Errorf("tool arguments exceed the 1 MiB limit")
	case !strings.HasPrefix(args, "{") || !json.Valid([]byte(args)):
		err = fmt.Errorf("tool arguments must be a JSON object")
	default:
		content, err = tool.Call(ctx, json.RawMessage(args))
	}
	event["durationMs"] = time.Since(start).Milliseconds()
	event["outcome"] = "success"
	if err != nil {
		event["outcome"] = "error"
		content = "MCP tool error: " + err.Error()
	}
	if len(content) > MaxToolOutput {
		const suffix = "\n[MCP result truncated]"
		content = strings.ToValidUTF8(content[:MaxToolOutput-len(suffix)], "") + suffix
	}
	if err := s.log.Event("tool-result", event); err != nil {
		return fireworks.Message{}, err
	}
	return fireworks.Message{Role: "tool", ToolCallID: call.ID, Content: content}, nil
}
