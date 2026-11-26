package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tichopad/bobik/internal/audit"
	"github.com/tichopad/bobik/internal/fireworks"
	"github.com/tichopad/bobik/internal/notes"
	"github.com/tichopad/bobik/internal/sandbox"
)

type completeFunc func(context.Context, fireworks.Request) (fireworks.Response, error)

func (f completeFunc) Complete(ctx context.Context, req fireworks.Request) (fireworks.Response, error) {
	return f(ctx, req)
}

type viewFunc func(context.Context, interface {
	Read(string, int64) ([]byte, error)
}, string, string, string) (string, fireworks.Usage, error)

func (f viewFunc) ViewMedia(ctx context.Context, source interface {
	Read(string, int64) ([]byte, error)
}, model, path, prompt string) (string, fireworks.Usage, error) {
	return f(ctx, source, model, path, prompt)
}

func config(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	store, err := notes.NewVault(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return Config{
		Notes: store, Prompt: "Update notes.", PromptPath: "prompt.txt",
		Model: "test-model", Now: time.Date(2026, 10, 19, 17, 55, 0, 123e6, time.UTC),
	}
}

func auditEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func types(events []map[string]any) []string {
	var names []string
	for _, event := range events {
		names = append(names, event["type"].(string))
	}
	return names
}

func TestRunToolHistoryAndStreamingAudit(t *testing.T) {
	cfg := config(t)
	cfg.Trigger = "@bobik"
	cfg.ChangedFiles = []ChangedFile{{"daily/old.md", true}}
	var writes []string
	cfg.OnWrite = func(path string) {
		writes = append(writes, path)
		events := auditEvents(t, filepath.Join(cfg.Notes.Base(), audit.Directory, audit.Filename(cfg.Now)))
		if events[len(events)-1]["type"] != "tool-call" {
			t.Fatal("tool was not audited before mutating")
		}
	}
	step := 0
	cfg.ModelClient = completeFunc(func(ctx context.Context, req fireworks.Request) (fireworks.Response, error) {
		step++
		if step == 1 {
			if len(req.Tools) != 6 || strings.Contains(req.Messages[0].Content, "October 19") ||
				!strings.Contains(req.Messages[1].Content, `Trigger: changes to 1 file matching "@bobik":`+"\ndaily/old.md (deleted)") {
				t.Fatalf("invalid initial request: %+v", req)
			}
			return fireworks.Response{
				Message: fireworks.Message{
					Reasoning: "write then edit",
					ToolCalls: []fireworks.Call{
						{ID: "1", Name: "writeFile", Arguments: `{"path":"/daily/a.md","content":"one one"}`},
						{ID: "2", Name: "editFile", Arguments: `{"path":"daily/a.md","oldString":"one","newString":"$&","replaceAll":true}`},
						{ID: "3", Name: "readFile", Arguments: `{"path":"daily/a.md"}`},
					},
				}, FinishReason: "tool_calls", Usage: fireworks.Usage{InputTokens: 3, OutputTokens: 4},
			}, nil
		}
		if len(req.Messages) != 6 || req.Messages[2].Reasoning != "write then edit" || len(req.Messages[2].ToolCalls) != 3 {
			t.Fatalf("incomplete request history: %+v", req.Messages)
		}
		for i, want := range []string{`{"success":true}`, `{"replacements":2,"success":true}`, `{"content":"$\u0026 $\u0026"}`} {
			got := req.Messages[i+3]
			if got.Role != "tool" || got.ToolCallID != fmt.Sprint(i+1) || got.Content != want {
				t.Fatalf("result %d = %+v, want %s", i, got, want)
			}
		}
		return fireworks.Response{Message: fireworks.Message{Content: "Done."}, FinishReason: "stop",
			Usage: fireworks.Usage{InputTokens: 5, OutputTokens: 6, TotalTokens: 11}}, nil
	})
	summary, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Text != "Done." || summary.Usage.TotalTokens != 18 || !slices.Equal(summary.WrittenPaths, []string{"daily/a.md"}) {
		t.Fatalf("incorrect summary: %+v", summary)
	}
	abs := filepath.Join(cfg.Notes.Base(), "daily/a.md")
	if !slices.Equal(writes, []string{abs, abs}) {
		t.Fatalf("writes = %q", writes)
	}
	if got, want := FormatSummary(summary), "bobik: 2 steps, 1 file written, 18 tokens (in 8, out 10), log "+summary.LogRelativePath; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	events := auditEvents(t, summary.LogPath)
	if got, want := types(events), []string{
		"run-start", "assistant-text", "tool-call", "tool-result", "tool-call", "tool-result",
		"tool-call", "tool-result", "step-finish", "assistant-text", "step-finish", "run-end",
	}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if events[3]["input"] == nil || events[len(events)-1]["status"] != "success" {
		t.Fatalf("invalid audit: %#v", events)
	}
}

func TestToolErrorsRecoverAndCustomTriggerProtectsDrafts(t *testing.T) {
	cfg := config(t)
	cfg.Trigger = "@draft"
	if err := cfg.Notes.Write("draft.md", []byte("%% @draft %%")); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Notes.Write("sub/missing.md", []byte("exists")); err != nil {
		t.Fatal(err)
	}
	secret := "private-provider-request-contents"
	cfg.ModelClient = completeFunc(func(ctx context.Context, req fireworks.Request) (fireworks.Response, error) {
		if len(req.Messages) == 2 {
			return fireworks.Response{Message: fireworks.Message{ToolCalls: []fireworks.Call{
				{ID: "1", Name: "deleteFile", Arguments: `{"path":"draft.md"}`},
				{ID: "2", Name: "writeFile", Arguments: `{"path":"should-not-exist.md","content":"data","path":"duplicate"}`},
				{ID: "3", Name: "unknown", Arguments: secret},
				{ID: "4", Name: "readFile", Arguments: `{"path":"missing.md"}`},
			}}, FinishReason: "tool_calls"}, nil
		}
		want := []string{"draft", "duplicate", "unknown tool", "missing.md"}
		for i, msg := range req.Messages[3:] {
			if !strings.Contains(msg.Content, want[i]) || !strings.Contains(msg.Content, `"error":`) {
				t.Errorf("unexpected result %d: %+v", i, msg)
			}
		}
		return fireworks.Response{}, errors.New(secret)
	})
	summary, err := Run(t.Context(), cfg)
	if !errors.Is(err, ErrModel) || strings.Contains(err.Error(), secret) || summary.Steps != 1 || summary.FilesWritten != 0 {
		t.Fatalf("unexpected failure: %+v, %v", summary, err)
	}
	events := auditEvents(t, summary.LogPath)
	if events[len(events)-1]["status"] != "error" || strings.Contains(fmt.Sprint(events), secret) {
		t.Fatalf("unexpected audit: %v", events)
	}
}

func TestPartialSummariesOnLimitAndDeadline(t *testing.T) {
	cfg := config(t)
	cfg.MaxSteps = 1
	cfg.ModelClient = completeFunc(func(context.Context, fireworks.Request) (fireworks.Response, error) {
		return fireworks.Response{Message: fireworks.Message{
			ToolCalls: []fireworks.Call{{ID: "write", Name: "writeFile", Arguments: `{"path":"new.md","content":"new"}`}},
		}, FinishReason: "tool_calls"}, nil
	})
	summary, err := Run(t.Context(), cfg)
	if !errors.Is(err, ErrStepLimit) || summary.Steps != 1 || summary.FilesWritten != 1 {
		t.Fatalf("limit result: %+v, %v", summary, err)
	}
	cfg.Timeout = time.Millisecond
	cfg.ModelClient = completeFunc(func(ctx context.Context, _ fireworks.Request) (fireworks.Response, error) {
		<-ctx.Done()
		return fireworks.Response{Usage: fireworks.Usage{InputTokens: 7}}, ctx.Err()
	})
	next, err := Run(t.Context(), cfg)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") || next.Steps != 0 || next.LogPath == summary.LogPath {
		t.Fatalf("deadline result: %+v, %v", next, err)
	}
}

func TestVisionFailureStillCountsUsageWithoutExposingProviderError(t *testing.T) {
	cfg := config(t)
	cfg.VisionModel = "vision"
	cfg.VisionClient = viewFunc(func(_ context.Context, _ interface {
		Read(string, int64) ([]byte, error)
	}, model, path, prompt string) (string, fireworks.Usage, error) {
		if model != "vision" || path != "pic.png" || prompt != fireworks.DefaultMediaPrompt {
			t.Fatalf("vision input: %q, %q, %q", model, path, prompt)
		}
		return "", fireworks.Usage{InputTokens: 13, OutputTokens: 2, TotalTokens: 15}, errors.New("data:image/png;base64,sensitive")
	})
	cfg.ModelClient = completeFunc(func(_ context.Context, req fireworks.Request) (fireworks.Response, error) {
		if len(req.Messages) == 2 {
			return fireworks.Response{Message: fireworks.Message{ToolCalls: []fireworks.Call{
				{ID: "media", Name: "viewMedia", Arguments: `{"path":"pic.png","prompt":" "}`},
			}}, FinishReason: "tool_calls"}, nil
		}
		if !strings.Contains(req.Messages[3].Content, "No fallback") || strings.Contains(req.Messages[3].Content, "sensitive") {
			t.Fatal(req.Messages[3].Content)
		}
		return fireworks.Response{Message: fireworks.Message{Content: "Inspection failed."}}, nil
	})
	summary, err := Run(t.Context(), cfg)
	if err != nil || summary.Usage.TotalTokens != 15 {
		t.Fatalf("vision result: %+v, %v", summary, err)
	}
	events := auditEvents(t, summary.LogPath)
	if !slices.Contains(types(events), "vision-inspection") || strings.Contains(fmt.Sprint(events), "sensitive") {
		t.Fatalf("unexpected audit: %v", events)
	}
}

func TestIncompleteFinishDoesNotDispatchToolCalls(t *testing.T) {
	for _, finish := range []string{"length", "content_filter", "error", "stop", ""} {
		t.Run(finish, func(t *testing.T) {
			cfg := config(t)
			cfg.ModelClient = completeFunc(func(context.Context, fireworks.Request) (fireworks.Response, error) {
				return fireworks.Response{FinishReason: finish, Message: fireworks.Message{ToolCalls: []fireworks.Call{
					{ID: "unsafe", Name: "writeFile", Arguments: `{"path":"unexpected.md","content":"write"}`},
				}}}, nil
			})
			summary, err := Run(t.Context(), cfg)
			if err == nil || summary.Steps != 1 || summary.FilesWritten != 0 {
				t.Fatalf("ambiguous finish: %+v, %v", summary, err)
			}
			if _, err := os.Stat(filepath.Join(cfg.Notes.Base(), "unexpected.md")); !os.IsNotExist(err) {
				t.Fatalf("tool mutated the vault: %v", err)
			}
		})
	}
}

func TestHistoryLimitStopsBeforeProviderCall(t *testing.T) {
	cfg := config(t)
	cfg.Prompt = strings.Repeat("a", MaxHistory)
	cfg.ModelClient = completeFunc(func(context.Context, fireworks.Request) (fireworks.Response, error) {
		t.Fatal("oversized history was sent to the provider")
		return fireworks.Response{}, nil
	})
	summary, err := Run(t.Context(), cfg)
	if !errors.Is(err, ErrHistoryLimit) || summary.Steps != 0 || summary.LogPath == "" {
		t.Fatalf("history limit: %+v, %v", summary, err)
	}
}

func TestAuditPathIsReadOnlyToTools(t *testing.T) {
	cfg := config(t)
	cfg.ModelClient = completeFunc(func(_ context.Context, req fireworks.Request) (fireworks.Response, error) {
		if len(req.Messages) == 2 {
			return fireworks.Response{FinishReason: "tool_calls", Message: fireworks.Message{ToolCalls: []fireworks.Call{
				{ID: "audit-read", Name: "readFile", Arguments: `{"path":".agent-bobik/2026-10-19T17-55-00.123.jsonl"}`},
				{ID: "audit-delete", Name: "deleteFile", Arguments: `{"path":"/.agent-bobik/2026-10-19T17-55-00.123.jsonl"}`},
			}}}, nil
		}
		if result := req.Messages[3].Content; !strings.Contains(result, "audit-read") {
			t.Errorf("audit read did not return the logged tool call: %s", result)
		}
		if result := req.Messages[4].Content; !strings.Contains(result, "not allowed") {
			t.Errorf("audit delete was not denied: %s", result)
		}
		return fireworks.Response{FinishReason: "stop"}, nil
	})
	summary, err := Run(t.Context(), cfg)
	if err != nil || summary.FilesWritten != 0 {
		t.Fatalf("read-only audit: %+v, %v", summary, err)
	}
}

func TestBashUsesLiveVaultAndRecordedRules(t *testing.T) {
	vault := t.TempDir()
	for _, dir := range []string{"private", "archive"} {
		if err := os.Mkdir(filepath.Join(vault, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := notes.NewVault(vault, []notes.Rule{
		{Path: "private", Mode: notes.Hide}, {Path: "archive", Mode: notes.ReadOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	preflight := sandbox.Preflight(t.Context())
	cfg := Config{Notes: store, Model: "test-model"}
	cfg.ModelClient = completeFunc(func(_ context.Context, req fireworks.Request) (fireworks.Response, error) {
		if len(req.Messages) == 2 {
			return fireworks.Response{FinishReason: "tool_calls", Message: fireworks.Message{ToolCalls: []fireworks.Call{
				{ID: "shell", Name: "bash", Arguments: `{"command":"set -- .agent-bobik/*.jsonl; IFS= read -r line < \"$1\" || exit 41; if printf x >private/x || printf x >archive/x || printf x >> \"$1\"; then exit 42; fi; printf live >live.md"}`},
			}}}, nil
		}
		result := req.Messages[len(req.Messages)-1].Content
		want := `"exitCode":0`
		if preflight != nil {
			want = "Bash sandbox unavailable"
		}
		if !strings.Contains(result, want) {
			t.Errorf("Bash result = %s, want %s", result, want)
		}
		return fireworks.Response{FinishReason: "stop"}, nil
	})
	summary, err := Run(t.Context(), cfg)
	if err != nil || len(summary.WrittenPaths) != 0 {
		t.Fatalf("Bash paths must not be attributed individually: %+v, %v", summary, err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "live.md"))
	if preflight == nil {
		if err != nil || string(data) != "live" {
			t.Fatalf("Bash write not visible on host: %q, %v", data, err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("command ran despite unavailable sandbox: %q, %v", data, err)
	}
}

func TestShellResultPreservesStatusWithinJSONBudget(t *testing.T) {
	text := strings.Repeat("x", 64<<10)
	out := shellOutput(sandbox.Result{Stdout: text, Stderr: text, ExitCode: 1})
	if out["stdout"] != text || out["stderr"] != text || out["exitCode"] != 1 ||
		out["stdoutTruncated"] != false || out["stderrTruncated"] != false {
		t.Fatal("ordinary output should not be truncated")
	}
	text = strings.Repeat("<💎\x00", 10_000)
	out = shellOutput(sandbox.Result{Stdout: text, Stderr: text, ExitCode: 42})
	data, err := json.Marshal(out)
	if err != nil || len(data) > MaxToolOutput || out["exitCode"] != 42 ||
		out["stdoutTruncated"] != true || out["stderrTruncated"] != true {
		t.Fatalf("encoded shell result: %d bytes, %v", len(data), err)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		if got := out[stream].(string); !utf8.ValidString(got) || !strings.HasPrefix(text, got) {
			t.Fatalf("%s was cut incorrectly", stream)
		}
	}
}

func TestRunThroughFireworksHTTP(t *testing.T) {
	cfg := config(t)
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
				Content    any    `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if req.Model != cfg.Model {
			t.Errorf("model = %q", req.Model)
		}
		if count == 1 {
			fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"w1","type":"function","function":{"name":"writeFile","arguments":"{\"path\":\"a.md\",\"content\":\"body\"}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
			return
		}
		if got := req.Messages[len(req.Messages)-1]; got.Role != "tool" || got.ToolCallID != "w1" || !reflect.DeepEqual(got.Content, `{"success":true}`) {
			t.Errorf("tool response = %+v", got)
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Saved."}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	defer server.Close()
	cfg.ModelClient = &fireworks.Client{Endpoint: server.URL, HTTP: server.Client()}
	summary, err := Run(t.Context(), cfg)
	if err != nil || summary.Text != "Saved." || summary.Usage.TotalTokens != 16 || summary.FilesWritten != 1 {
		t.Fatalf("HTTP run: %+v, %v", summary, err)
	}
}
