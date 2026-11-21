package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixture(calls *atomic.Int32) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, &mcp.ServerOptions{PageSize: 1})
	for _, name := range []string{"disable", "echo"} {
		s.AddTool(&mcp.Tool{Name: name, Description: "echo arguments", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			return &mcp.CallToolResult{
				IsError:           true,
				Content:           []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}, &mcp.ImageContent{MIMEType: "image/png", Data: []byte("hidden")}},
				StructuredContent: map[string]string{"value": "structured"},
			}, nil
		})
	}
	return s
}

func TestHTTPDiscoveryAndCall(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run("legacy="+strconv.FormatBool(legacy), func(t *testing.T) {
			var calls atomic.Int32
			s := fixture(&calls)
			get := func(*http.Request) *mcp.Server { return s }
			var h http.Handler = mcp.NewStreamableHTTPHandler(get, nil)
			if legacy {
				h = mcp.NewSSEHandler(get, nil)
			}
			addr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer supplied" {
					http.Error(w, "expected auth", 401)
					return
				}
				if legacy && r.Method == "POST" && r.URL.RawQuery == "" {
					w.WriteHeader(405)
					return
				}
				h.ServeHTTP(w, r)
			}))
			defer addr.Close()
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer supplied")
				return addr.Client().Transport.RoundTrip(r)
			})}
			ctx, cancel := context.WithCancel(t.Context())
			server, err := Discover(ctx, Config{URL: addr.URL, DisabledTools: []string{"disable"}}, hc)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			if tools := server.Tools(); len(tools) != 1 || tools[0].Name != "echo" || string(tools[0].InputSchema) != `{"type":"object"}` {
				t.Fatalf("tools: %+v", tools)
			}
			args := json.RawMessage(`{"number":9007199254740993}`)
			if _, err := server.Call(t.Context(), "disable", args); err == nil {
				t.Fatal("disabled call succeeded")
			}
			got, err := server.Call(t.Context(), "echo", args)
			if err != nil || !got.IsError || got.Text != string(args)+"\n"+`{"value":"structured"}`+"\n[non-text content omitted]" || calls.Load() != 1 {
				t.Fatalf("call: %+v, %v; requests=%d", got, err, calls.Load())
			}
		})
	}
}

func TestStatusFallback(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 405, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var posts, gets atomic.Int32
			addr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "POST":
					posts.Add(1)
				case "GET":
					gets.Add(1)
				}
				http.Error(w, "private-body", status)
			}))
			defer addr.Close()
			_, err := Discover(t.Context(), Config{URL: addr.URL + "?token=private-token"}, addr.Client())
			if errors.Is(err, ErrUnauthorized) != (status == 401 || status == 403) {
				t.Fatalf("status %d, discovery error: %v", status, err)
			}
			if err == nil || strings.Contains(err.Error(), "private") || posts.Load() != 1 || (gets.Load() == 1) != (status == 404 || status == 405) {
				t.Fatalf("err=%v posts=%d gets=%d", err, posts.Load(), gets.Load())
			}
		})
	}
}

func TestStdioHelper(t *testing.T) {
	if os.Getenv("BOBIK_TEST_STDIO") == "" {
		return
	}
	cwd, err := os.Getwd()
	if _, ok := os.LookupEnv("BOBIK_TEST_PRIVATE"); ok || os.Getenv("PATH") == "" || os.Getenv("HOME") != "/test-home" || os.Getenv("TMPDIR") != "/test-tmp" || cwd != "/" || err != nil {
		os.Exit(2)
	}
	var calls atomic.Int32
	if err := fixture(&calls).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestStdio(t *testing.T) {
	t.Setenv("BOBIK_TEST_STDIO", "1")
	t.Setenv("BOBIK_TEST_PRIVATE", "secret")
	t.Setenv("HOME", "/test-home")
	t.Setenv("TMPDIR", "/test-tmp")
	s, err := Discover(t.Context(), Config{Command: os.Args[0], Args: []string{"-test.run=^TestStdioHelper$"}, PassEnv: []string{"BOBIK_TEST_STDIO"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Tools()) != 2 {
		t.Fatal(s.Tools())
	}
}

func TestStartupDeadline(t *testing.T) {
	block := make(chan struct{})
	addr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-block:
		}
	}))
	defer addr.Close()
	defer close(block)
	_, err := Discover(t.Context(), Config{URL: addr.URL, Timeout: 100 * time.Millisecond}, addr.Client())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup error: %v", err)
	}
}

func TestCallFailureDoesNotReplay(t *testing.T) {
	for _, status := range []int{307, 401, 403, 500, 504} {
		timeout := status == 504
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var count atomic.Int32
			s := fixture(&count)
			h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
			release := make(chan struct{})
			addr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				var msg struct{ Method string }
				_ = json.Unmarshal(body, &msg)
				if msg.Method == "tools/call" {
					count.Add(1)
					if timeout {
						select {
						case <-release:
						case <-r.Context().Done():
						}
					}
					w.Header().Set("Location", "/moved")
					http.Error(w, "private details", status)
					return
				}
				h.ServeHTTP(w, r)
			}))
			defer addr.Close()
			defer close(release)
			server, err := Discover(t.Context(), Config{URL: addr.URL + "?token=private-token"}, addr.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			server.timeout = 150 * time.Millisecond
			_, err = server.Call(t.Context(), "echo", json.RawMessage(`{}`))
			if got, want := errors.Is(err, ErrUnauthorized), status == 401 || status == 403; got != want {
				t.Fatalf("status %d, call error: %v", status, err)
			}
			if err == nil || (!errors.Is(err, ErrUnauthorized) && !strings.Contains(err.Error(), "outcome unknown")) || strings.Contains(err.Error(), "private") || count.Load() != 1 || timeout != errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("calls=%d err=%v", count.Load(), err)
			}
		})
	}
}

func TestBoundResults(t *testing.T) {
	for _, r := range []*mcp.CallToolResult{
		{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("界", maxResult)}}},
		{StructuredContent: map[string]string{"blob": strings.Repeat("界", maxResult)}},
	} {
		r.Content = append(r.Content, &mcp.ImageContent{Data: []byte("hidden")})
		got, err := render(r)
		if err != nil || !utf8.ValidString(got.Text) || len(got.Text) > maxResult || !strings.HasSuffix(got.Text, "[truncated]\n[non-text content omitted]") {
			t.Fatalf("bad bounded result: bytes=%d err=%v", len(got.Text), err)
		}
	}
}
