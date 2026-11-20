package fireworks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete cancellation = %v", err)
	}
}

func TestCompleteSanitizesUnusableResponses(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":6,"completion_tokens":4}`
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		errorText  string
		wantTokens int
	}{
		{"status", 429, `{"error":"provider-secret"}`, "HTTP 429", 0},
		{"body", 200, `{"error":{"message":"provider-secret"},` + usage + `}`, "provider error", 10},
		{"empty choices", 200, `{"choices":[],` + usage + `}`, "no completion message", 10},
		{"null message", 200, `{"choices":[{"message":null}],` + usage + `}`, "no completion message", 10},
		{"invalid JSON", 200, `provider-secret`, "invalid Fireworks response", 0},
		{"response limit", 200, strings.Repeat("a", 4<<20+1), "4 MiB limit", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			resp, err := client.Complete(t.Context(), fireworks.Request{})
			if err == nil || !strings.Contains(err.Error(), tc.errorText) ||
				strings.Contains(err.Error(), "provider-secret") || strings.Contains(err.Error(), "test-key") ||
				errors.Unwrap(err) != nil || resp.Usage.TotalTokens != tc.wantTokens {
				t.Fatalf("Complete() = %#v, %v; want safe %q error and %d tokens", resp, err, tc.errorText, tc.wantTokens)
			}
		})
	}
}

func TestCompleteKeepsErrorFinishWithoutLoggingProviderMessage(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"choices":[{"finish_reason":"error","message":{"content":"provider-secret","reasoning_content":"provider-secret"}}],
			"usage":{"total_tokens":7}
		}`)
	})
	resp, err := client.Complete(t.Context(), fireworks.Request{})
	want := fireworks.Response{
		Message:      fireworks.Message{Role: "assistant"},
		FinishReason: "error",
		Usage:        fireworks.Usage{TotalTokens: 7},
	}
	if err != nil || !reflect.DeepEqual(resp, want) {
		t.Fatalf("Complete() = %#v, %v; want %#v", resp, err, want)
	}
}

func TestCompleteDoesNotForwardRedirect(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/redirect" {
			http.Error(w, "unexpected redirected request", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/redirect", http.StatusFound)
	})
	_, err := client.Complete(t.Context(), fireworks.Request{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") || calls.Load() != 1 {
		t.Fatalf("Complete() error = %v, after %d requests", err, calls.Load())
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("transport-secret: %s; %s", r.URL, r.Header.Get("Authorization"))
}

func TestCompleteSanitizesTransportError(t *testing.T) {
	client := &fireworks.Client{
		APIKey: "key-secret", Endpoint: "http://example.invalid/?token=url-secret",
		HTTP: &http.Client{Transport: failingTransport{}},
	}
	_, err := client.Complete(t.Context(), fireworks.Request{})
	if err == nil || err.Error() != "Fireworks request failed" || errors.Unwrap(err) != nil {
		t.Fatalf("Complete() error = %v", err)
	}
}
