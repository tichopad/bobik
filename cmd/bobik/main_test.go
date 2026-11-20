package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tichopad/bobik/internal/fireworks"
)

func cliFixture(t *testing.T, server *httptest.Server) (app, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "prompt"), []byte("Maintain test notes."), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	a := app{
		stdout: &out, stderr: &errOut,
		cwd: func() (string, error) { return cwd, nil },
		getenv: func(k string) string {
			if k == "FIREWORKS_API_KEY" {
				return "test-secret"
			}
			return ""
		},
		client: func(key string) *fireworks.Client {
			if server == nil {
				t.Fatal("unexpected model request")
			}
			return &fireworks.Client{APIKey: key, Endpoint: server.URL, HTTP: server.Client()}
		},
	}
	return a, cwd, &out, &errOut
}

func TestCLICompletionThroughHTTPAndAudit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("unexpected authorization header")
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Model != "test-model" || len(req.Tools) == 0 {
			t.Errorf("wrong model or missing tool schemas: %+v", req)
		}
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"create","type":"function","function":{"name":"writeFile","arguments":"{\"path\":\"new.md\",\"content\":\"# New note\\n\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
		case 2:
			if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].ToolCallID != "create" {
				t.Error("missing linked tool response")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Created a note."},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`)
		default:
			t.Error("too many model requests")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	a, cwd, stdout, stderr := cliFixture(t, server)
	if got := a.run([]string{"--prompt", "prompt", "--allow-path", "*.md", "--model", "test-model"}); got != 0 {
		t.Fatalf("exit %d, stderr %s", got, stderr)
	}
	if stdout.String() != "Created a note.\n" || !strings.Contains(stderr.String(), "2 steps, 1 file written, 18 tokens") {
		t.Fatalf("stdout %q, stderr %q", stdout, stderr)
	}
	if b, err := os.ReadFile(filepath.Join(cwd, "new.md")); string(b) != "# New note\n" || err != nil {
		t.Fatalf("written note %q, error %v", b, err)
	}
	logs, err := filepath.Glob(filepath.Join(cwd, ".agent-bobik", "*.jsonl"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs %v, error %v", logs, err)
	}
	b, err := os.ReadFile(logs[0])
	if err != nil || bytes.Contains(b, []byte("test-secret")) {
		t.Fatalf("unreadable or unsafe audit: %v", err)
	}
	var events []string
	scan := bufio.NewScanner(bytes.NewReader(b))
	for scan.Scan() {
		var line struct{ Type string }
		if err := json.Unmarshal(scan.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		events = append(events, line.Type)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	order := strings.Join(events, ",")
	if !strings.HasPrefix(order, "run-start,tool-call,tool-result,step-finish,") || !strings.HasSuffix(order, ",run-end") {
		t.Fatalf("audit event order: %s", order)
	}
}

func TestCLIRuntimeFailureStillReportsAudit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret raw provider body", http.StatusBadGateway)
	}))
	defer server.Close()
	a, _, stdout, stderr := cliFixture(t, server)
	if got := a.run([]string{"--prompt=prompt", "--allow-path=*.md"}); got != exitRunError {
		t.Fatalf("exit %d, stderr %s", got, stderr)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), ".agent-bobik/") || strings.Contains(stderr.String(), "secret raw") {
		t.Fatalf("stdout %q, stderr %q", stdout, stderr)
	}
}

func TestCLIConfigIsNotAReadableNote(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"read","type":"function","function":{"name":"readFile","arguments":"{\"path\":\"config/bobik/config.json\"}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2:
			b, err := io.ReadAll(r.Body)
			if err != nil || bytes.Contains(b, []byte("cfg-secret")) || !bytes.Contains(b, []byte("not allowed")) {
				t.Error("saved API key was not safely excluded")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		default:
			t.Error("unexpected additional model request")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	a, cwd, _, stderr := cliFixture(t, server)
	a.getenv = func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return filepath.Join(cwd, "config")
		}
		return ""
	}
	if _, err := saveKey("cfg-secret", a.getenv); err != nil {
		t.Fatal(err)
	}
	if code := a.run([]string{"--prompt=prompt", "--allow-path=**/*.json"}); code != 0 || requests.Load() != 2 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
}

func TestCLIMigrationAndUsage(t *testing.T) {
	a, _, stdout, stderr := cliFixture(t, nil)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--deny", "rm"}, "was removed"},
		{[]string{"--prompt", "absent", "--allow-path", "*.md"}, "prompt file not found"},
		{[]string{"--prompt", "prompt", "--allow-path", "../*.md"}, "allow"},
	} {
		stderr.Reset()
		if got := a.run(tc.args); got != exitUsage || !strings.Contains(stderr.String(), tc.want) || stdout.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", tc.args, got, stdout, stderr)
		}
	}
}

func TestCLIWatchSignals(t *testing.T) {
	for _, force := range []bool{false, true} {
		name, want := "graceful", exitOK
		if force {
			name, want = "second signal", exitInterrupted
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-release:
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer close(release)
			a, _, stdout, _ := cliFixture(t, server)
			signals := make(chan os.Signal, 2)
			a.signals = signals
			done := make(chan int, 1)
			go func() { done <- a.run([]string{"--prompt=prompt", "--allow-path=*.md", "--watch"}) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("startup request not received")
			}
			signals <- syscall.SIGTERM
			if force {
				signals <- syscall.SIGINT
			} else {
				release <- struct{}{}
			}
			select {
			case code := <-done:
				if code != want || (!force && stdout.String() != "done\n") {
					t.Fatalf("exit %d, want %d; stdout %q", code, want, stdout)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("watch did not stop")
			}
		})
	}
}
