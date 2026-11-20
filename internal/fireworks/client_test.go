package fireworks_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/tichopad/bobik/internal/fireworks"
)

func testClient(t *testing.T, handler http.HandlerFunc) *fireworks.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &fireworks.Client{
		Endpoint: server.URL + "/inference/v1/chat/completions",
		APIKey:   "test-key",
		HTTP:     server.Client(),
	}
}

func TestCompleteToolConversation(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/inference/v1/chat/completions" ||
			r.Header.Get("Authorization") != "Bearer test-key" ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("wrong request: %s %s %v", r.Method, r.URL.Path, r.Header)
		}
		const want = `{
			"model":"accounts/fireworks/models/test", "max_tokens":128, "stream":false,
			"messages":[
				{"role":"system","content":"instructions"},
				{"role":"user","content":"task"},
				{"role":"assistant","content":null,"reasoning_content":"check the note",
				 "tool_calls":[{"id":"call_1","type":"function","function":{"name":"readFile","arguments":"{\"path\":\"note.md\"}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"note text"}
			],
			"tools":[{"type":"function","function":{"name":"readFile","description":"Read a note",
				"parameters":{"type":"object","additionalProperties":false}}}]
		}`
		var body, expected any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal([]byte(want), &expected); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(body, expected) {
			t.Errorf("request body = %#v, want %#v", body, expected)
		}
		_, _ = io.WriteString(w, `{
			"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
			  "reasoning_content":"now edit",
			  "tool_calls":[{"id":"call_2","type":"function","function":{"name":"editFile","arguments":"{invalid"}}]}}],
			"usage":{"prompt_tokens":45,"completion_tokens":19,"total_tokens":64,"other":"not for audit"}
		}`)
	})

	got, err := client.Complete(t.Context(), fireworks.Request{
		Model: "accounts/fireworks/models/test", MaxTokens: 128,
		Messages: []fireworks.Message{
			{Role: "system", Content: "instructions"},
			{Role: "user", Content: "task"},
			{Role: "assistant", Reasoning: "check the note", ToolCalls: []fireworks.Call{
				{ID: "call_1", Name: "readFile", Arguments: `{"path":"note.md"}`},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "note text"},
		},
		Tools: []fireworks.Tool{{
			Name: "readFile", Description: "Read a note", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		}},
	})
	want := fireworks.Response{
		Message: fireworks.Message{
			Role: "assistant", Reasoning: "now edit",
			ToolCalls: []fireworks.Call{{ID: "call_2", Name: "editFile", Arguments: "{invalid"}},
		},
		FinishReason: "tool_calls",
		Usage:        fireworks.Usage{InputTokens: 45, OutputTokens: 19, TotalTokens: 64},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Complete() = %#v, %v; want %#v", got, err, want)
	}
	audit, err := json.Marshal(got.Usage)
	if err != nil || string(audit) != `{"inputTokens":45,"outputTokens":19,"totalTokens":64}` {
		t.Fatalf("audit usage = %s, %v", audit, err)
	}
}

func TestCompleteCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, err := client.Complete(ctx, fireworks.Request{})
	if err == nil {
		t.Fatal("Complete returned success after cancellation")
	}
}
