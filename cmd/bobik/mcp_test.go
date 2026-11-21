package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPPerRunDiscoveryAndHeaders(t *testing.T) {
	var initialized, closed atomic.Int32
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{InitializedHandler: func(context.Context, *mcp.InitializedRequest) { initialized.Add(1) }})
	for _, name := range []string{"hidden", "visible"} {
		mcpServer.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "use another query"}}}, nil
		})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-header" {
			http.Error(w, "private-error", 401)
			return
		}
		if r.Method == http.MethodDelete {
			closed.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	var cfg settings
	err := json.Unmarshal([]byte(fmt.Sprintf(`{"mcpServers":{"a":{"command":"/nonexistent/bobik-mcp"},"b":{"url":%q,"headersEnv":{"Authorization":"AUTH"},"disabledTools":["hidden"]}}}`, server.URL)), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	open := openMCP(cfg.MCPServers, t.TempDir(), &warnings, func(k string) string {
		if k == "AUTH" {
			return "Bearer private-header"
		}
		return ""
	})
	for range 2 {
		tools, closeSessions := open(t.Context())
		if len(tools) != 1 || tools[0].Server != "b" || tools[0].Name != "visible" {
			closeSessions()
			t.Fatalf("discovered %d; warnings: %s", len(tools), warnings.String())
		}
		_, err := tools[0].Call(t.Context(), json.RawMessage(`{}`))
		closeSessions()
		if err == nil || !strings.Contains(err.Error(), "use another query") {
			t.Fatal("model did not receive tool error", err)
		}
	}
	if initialized.Load() != 2 || closed.Load() != 2 || strings.Count(warnings.String(), "discovery failed") != 2 || strings.Contains(warnings.String(), "private") {
		t.Fatalf("initialized=%d closed=%d warnings=%s", &initialized, &closed, warnings.String())
	}
}

func TestCLIRejectsExposedCredentialsWithoutOAuthServers(t *testing.T) {
	for _, kind := range []string{"default config", "OAuth state"} {
		t.Run(kind, func(t *testing.T) {
			a, cwd, _, stderr := cliFixture(t, nil)
			path := filepath.Join(cwd, "chosen.json")
			if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(cwd, "vault", "credentials")
			if kind == "OAuth state" {
				t.Setenv("XDG_STATE_HOME", root)
				if err := os.MkdirAll(filepath.Join(root, "bobik", "mcpauth"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				a.getenv = func(k string) string {
					switch k {
					case "XDG_CONFIG_HOME":
						return root
					case "FIREWORKS_API_KEY":
						return "env-key"
					}
					return ""
				}
				if err := saveKey("sensitive", filepath.Join(root, "bobik", "config.json")); err != nil {
					t.Fatal(err)
				}
			}
			if code := a.run([]string{"--config", path, "--vault=vault", "--prompt=prompt"}); code != exitUsage || !strings.Contains(stderr.String(), "vault") {
				t.Fatalf("exit %d: %s", code, stderr)
			}
		})
	}
}

func TestLoginValidatesConfigWithoutFireworksKey(t *testing.T) {
	a, cwd, _, stderr := cliFixture(t, nil)
	a.getenv = func(string) string { return "" }
	path := filepath.Join(cwd, "config.json")
	for _, tc := range []struct{ server, want string }{
		{`{"command":"unused"}`, "OAuth-enabled"},
		{`{"url":"https://example.com","oauth":{"clientSecret":"private-value"}}`, "invalid bobik config"},
		{`{"url":"https://example.com","oauth":{"clientId":"id","clientSecretEnv":"UNSET"}}`, "variable is unset"},
	} {
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"mcpServers":{"test":%s}}`, tc.server)), 0600); err != nil {
			t.Fatal(err)
		}
		stderr.Reset()
		if code := a.run([]string{"mcp", "login", "test", "--config", path}); code != exitUsage || !strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "private-value") {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	}
}
