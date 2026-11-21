package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Set BOBIK_TEST_SEARXNG_CONFIG to a trusted config with a SearXNG server.
// The search is real; the model is local and needs no API key.
func TestLiveSearxng(t *testing.T) {
	path := os.Getenv("BOBIK_TEST_SEARXNG_CONFIG")
	if path == "" {
		t.Skip("BOBIK_TEST_SEARXNG_CONFIG not set")
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools    []struct{ Function struct{ Name string } }
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			t.Errorf("bad model request: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		if msg := req.Messages[len(req.Messages)-1]; msg.Role == "tool" {
			if !strings.Contains(msg.Content, "https://") {
				t.Errorf("search did not return a URL: %.300s", msg.Content)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Search completed."},"finish_reason":"stop"}]}`)
			return
		}
		for _, tool := range req.Tools {
			if strings.HasSuffix(tool.Function.Name, "__searxng_search") {
				fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"search","type":"function","function":{"name":%q,"arguments":"{\"query\":\"site:go.dev Go documentation\",\"max_results\":1}"}}]},"finish_reason":"tool_calls"}]}`, tool.Function.Name)
				return
			}
		}
		t.Error("model was not offered the SearXNG search tool")
		http.Error(w, "tool missing", 400)
	}))
	defer model.Close()
	a, _, stdout, stderr := cliFixture(t, model)
	a.getenv = func(key string) string {
		if key == "FIREWORKS_API_KEY" {
			return "local-test"
		}
		return os.Getenv(key)
	}
	if code := a.run([]string{"--config", path, "--prompt", "prompt", "--vault", "vault"}); code != exitOK || stdout.String() != "Search completed.\n" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
