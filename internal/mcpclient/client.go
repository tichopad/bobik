// Package mcpclient connects to one configured MCP tool server at a time.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Config struct {
	Command       string
	Args          []string
	PassEnv       []string
	URL           string
	DisabledTools []string
	Timeout       time.Duration
}

// ErrUnauthorized indicates rejected credentials or insufficient permissions.
var ErrUnauthorized = errors.New("MCP authorization denied")

type authObservationKey struct{}

func observeAuth(ctx context.Context) (context.Context, *atomic.Bool) {
	flag := new(atomic.Bool)
	return context.WithValue(ctx, authObservationKey{}, flag), flag
}

type Tool struct {
	Name, Description string
	InputSchema       json.RawMessage
}

type Result struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}

type Server struct {
	session *mcp.ClientSession
	cancel  context.CancelFunc
	timeout time.Duration
	tools   []Tool
	enabled map[string]bool
}

// Discover connects, initializes and lists all pages under one startup deadline.
func Discover(ctx context.Context, cfg Config, hc *http.Client) (*Server, error) {
	if (cfg.Command == "") == (cfg.URL == "") {
		return nil, errors.New("configure exactly one MCP command or URL")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	ctx, denied := observeAuth(ctx)
	startup, done := context.WithTimeout(ctx, cfg.Timeout)
	defer done()
	// The legacy SSE GET must outlive discovery, but must still be interruptible during startup.
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(startup, cancel)
	defer stop()
	s := &Server{cancel: cancel, timeout: cfg.Timeout, enabled: make(map[string]bool)}
	client := mcp.NewClient(&mcp.Implementation{Name: "bobik", Version: "1"}, &mcp.ClientOptions{Logger: slog.New(slog.DiscardHandler)})
	connect := func(tr mcp.Transport) (*mcp.ClientSession, error) {
		// Start with initialize; avoid a speculative server/discover POST.
		return client.Connect(life, tr, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	}
	var err error
	if cfg.Command != "" {
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Dir = "/"
		cmd.Env = []string{}
		for _, key := range append([]string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP"}, cfg.PassEnv...) {
			if val, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+val)
			}
		}
		s.session, err = connect(&mcp.CommandTransport{Command: cmd, TerminateDuration: min(cfg.Timeout, 200*time.Millisecond)})
	} else {
		endpoint, parseErr := url.Parse(cfg.URL)
		if parseErr != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
			cancel()
			return nil, errors.New("invalid MCP HTTP endpoint")
		}
		if hc == nil {
			hc = http.DefaultClient
		}
		c := *hc
		// A 307/308 redirect would replay the tool call.
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		base := hc.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		var firstStatus atomic.Int32
		c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// The SSE message endpoint comes from the peer; keep credentials on the configured origin.
			if r.URL.Scheme != endpoint.Scheme || !strings.EqualFold(r.URL.Host, endpoint.Host) {
				return nil, errors.New("cross-origin MCP endpoint")
			}
			resp, err := base.RoundTrip(r)
			if errors.Is(err, ErrUnauthorized) || (resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden)) {
				if flag, ok := r.Context().Value(authObservationKey{}).(*atomic.Bool); ok {
					flag.Store(true)
				}
			}
			if err == nil && r.Method == http.MethodPost {
				firstStatus.CompareAndSwap(0, int32(resp.StatusCode))
			}
			return resp, err
		})
		s.session, err = connect(&mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: &c, MaxRetries: -1, DisableStandaloneSSE: true})
		if err != nil && startup.Err() == nil && (firstStatus.Load() == 404 || firstStatus.Load() == 405) {
			s.session, err = connect(&mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: &c})
		}
	}
	if err != nil {
		err = errors.New("MCP connection or initialization failed")
	} else {
		err = s.discoverTools(startup, cfg.DisabledTools)
	}
	if !stop() || startup.Err() != nil {
		err = startup.Err()
	}
	if err != nil {
		if denied.Load() {
			err = ErrUnauthorized
		}
		cancel()
		if s.session != nil {
			_ = s.session.Close()
		}
		return nil, err
	}
	return s, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (s *Server) discoverTools(ctx context.Context, disabled []string) error {
	for t, err := range s.session.Tools(ctx, nil) {
		if err != nil {
			return errors.New("MCP tool discovery failed")
		}
		if t == nil || t.Name == "" {
			return errors.New("invalid unnamed MCP tool")
		}
		if slices.Contains(disabled, t.Name) {
			continue
		}
		if s.enabled[t.Name] {
			return fmt.Errorf("duplicate MCP tool %q", t.Name)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil || len(schema) == 0 || schema[0] != '{' {
			return fmt.Errorf("invalid input schema for %q", t.Name)
		}
		s.enabled[t.Name] = true
		s.tools = append(s.tools, Tool{t.Name, t.Description, schema})
	}
	return nil
}

func (s *Server) Tools() []Tool { return slices.Clone(s.tools) }

func (s *Server) Close() error {
	s.cancel()
	if err := s.session.Close(); err != nil {
		return errors.New("MCP session shutdown failed")
	}
	return nil
}

func (s *Server) Call(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	if !s.enabled[name] {
		return Result{}, fmt.Errorf("MCP tool %q unavailable", name)
	}
	args = bytes.TrimSpace(args)
	if !json.Valid(args) || args[0] != '{' {
		return Result{}, errors.New("MCP arguments must be a JSON object")
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	ctx, denied := observeAuth(ctx)
	r, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil && denied.Load() {
		return Result{}, ErrUnauthorized
	}
	if ctx.Err() != nil {
		return Result{}, fmt.Errorf("MCP call outcome unknown: %w", ctx.Err())
	}
	if err != nil {
		return Result{}, errors.New("MCP call failed; outcome unknown")
	}
	return render(r)
}

const maxResult = 32 << 10

func render(r *mcp.CallToolResult) (Result, error) {
	var b strings.Builder
	truncated := false
	omitted := false
	add := func(text string) {
		if text == "" || truncated {
			return
		}
		if b.Len() > 0 && b.Len() < maxResult {
			b.WriteByte('\n')
		}
		room := maxResult - b.Len()
		if len(text) > room {
			truncated = true
			text = text[:room]
		}
		b.WriteString(text)
	}
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			add(t.Text)
		} else {
			omitted = true
		}
	}
	if r.StructuredContent != nil && !truncated {
		data, err := json.Marshal(r.StructuredContent)
		if err != nil {
			return Result{}, err
		}
		add(string(data))
	}
	text := b.String()
	marker := ""
	if omitted {
		marker = "\n[non-text content omitted]"
	}
	if truncated || len(text)+len(marker) > maxResult {
		marker = "\n[truncated]" + marker
		n := maxResult - len(marker)
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		text = text[:n]
	}
	if len(text) == 0 {
		marker = strings.TrimPrefix(marker, "\n")
	}
	text += marker
	return Result{Text: text, IsError: r.IsError}, nil
}
