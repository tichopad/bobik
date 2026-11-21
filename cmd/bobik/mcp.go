package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tichopad/bobik/internal/agent"
	"github.com/tichopad/bobik/internal/mcpauth"
	"github.com/tichopad/bobik/internal/mcpclient"
)

const loginUsage = "usage: bobik mcp login <server> [--vault <directory>] [--config <path>]"

func (a app) login(args []string, override string) int {
	var name, vault string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--vault" && i+1 < len(args) && strings.TrimSpace(args[i+1]) != "" && !strings.HasPrefix(args[i+1], "--") && vault == "":
			i++
			vault = args[i]
		case strings.HasPrefix(arg, "--vault=") && strings.TrimSpace(strings.TrimPrefix(arg, "--vault=")) != "" && vault == "":
			vault = strings.TrimPrefix(arg, "--vault=")
		case name == "" && !strings.HasPrefix(arg, "-"):
			name = arg
		default:
			return a.fail(exitUsage, errors.New(loginUsage))
		}
	}
	if name == "" {
		return a.fail(exitUsage, errors.New(loginUsage))
	}
	cwd, err := a.cwd()
	if err != nil {
		return a.fail(exitRunError, err)
	}
	path, err := a.settingsPath(override)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	if vault != "" {
		vault, err = filepath.EvalSymlinks(fromDirectory(cwd, vault))
		if err != nil {
			return a.fail(exitUsage, err)
		}
		st, err := os.Stat(vault)
		if err != nil || !st.IsDir() {
			return a.fail(exitUsage, errors.New("--vault must name an existing directory"))
		}
		if err = checkKeyLocation(vault, path); err != nil {
			return a.fail(exitUsage, err)
		}
	}
	cfg, err := loadConfig[settings](path, false)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	s, ok := cfg.MCPServers[name]
	if !ok || s.URL == "" || s.OAuth == nil {
		return a.fail(exitUsage, errors.New("expected an OAuth-enabled remote MCP server"))
	}
	auth, err := s.authConfig(name, vault, a.getenv)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelOnSignal(ctx, cancel, a.signals)
	if err := mcpauth.Login(ctx, auth, mcpauth.LoginOptions{}); err != nil {
		return a.fail(exitRunError, err)
	}
	fmt.Fprintf(a.stdout, "Logged in to MCP server %q.\n", name)
	return exitOK
}

type serverSettings struct {
	Type           string            `json:"type"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	PassEnv        []string          `json:"passEnv"`
	URL            string            `json:"url"`
	HeadersEnv     map[string]string `json:"headersEnv"`
	DisabledTools  []string          `json:"disabledTools"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	OAuth          *oauthSettings    `json:"oauth"`
}

type oauthSettings struct {
	ClientID        string   `json:"clientId"`
	ClientSecretEnv string   `json:"clientSecretEnv"`
	RedirectURL     string   `json:"redirectUrl"`
	Scopes          []string `json:"scopes"`
}

func (s *serverSettings) UnmarshalJSON(b []byte) error {
	type raw serverSettings
	var cfg raw
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return err
	}
	if (cfg.URL == "") == (cfg.Command == "") || cfg.TimeoutSeconds < 0 || cfg.TimeoutSeconds > 300 {
		return errors.New("expected one command or URL and timeout of at most 300 seconds")
	}
	if cfg.URL == "" {
		if (cfg.Type != "" && cfg.Type != "stdio") || cfg.OAuth != nil || len(cfg.HeadersEnv) > 0 {
			return errors.New("invalid stdio settings")
		}
	} else {
		if (cfg.Type != "" && cfg.Type != "http") || len(cfg.Args) > 0 || len(cfg.PassEnv) > 0 || (cfg.OAuth != nil && len(cfg.HeadersEnv) > 0) {
			return errors.New("invalid HTTP settings")
		}
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("invalid HTTP URL")
		}
		if u.Scheme == "http" && len(cfg.HeadersEnv) > 0 && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
			return errors.New("authenticated MCP requires HTTPS or loopback")
		}
	}
	for _, env := range cfg.PassEnv {
		if !envName.MatchString(env) {
			return errors.New("invalid passEnv name")
		}
	}
	for header, env := range cfg.HeadersEnv {
		if !headerName.MatchString(header) || !envName.MatchString(env) {
			return errors.New("invalid header environment setting")
		}
	}
	if cfg.OAuth != nil && cfg.OAuth.ClientSecretEnv != "" && !envName.MatchString(cfg.OAuth.ClientSecretEnv) {
		return errors.New("invalid clientSecretEnv name")
	}
	*s = serverSettings(cfg)
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var headerName = regexp.MustCompile("^[a-zA-Z0-9!#$%&'*+.^_`|~-]+$")

func (s serverSettings) authConfig(name, vault string, getenv environment) (mcpauth.Config, error) {
	var secret string
	if s.OAuth.ClientSecretEnv != "" {
		secret = getenv(s.OAuth.ClientSecretEnv)
		if secret == "" {
			return mcpauth.Config{}, errors.New("OAuth client secret environment variable is unset")
		}
	}
	return mcpauth.Config{Name: name, VaultRoot: vault, ServerURL: s.URL, ClientID: s.OAuth.ClientID, ClientSecret: secret, RedirectURL: s.OAuth.RedirectURL, Scopes: s.OAuth.Scopes}, nil
}

type headerTransport struct {
	origin  *url.URL
	headers http.Header
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.origin.Scheme || !strings.EqualFold(req.URL.Host, t.origin.Host) || req.URL.User != nil {
		return nil, errors.New("refusing MCP credentials outside configured origin")
	}
	r := req.Clone(req.Context())
	for k := range t.headers {
		r.Header.Set(k, t.headers.Get(k))
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (s serverSettings) httpClient(ctx context.Context, name, vault string, getenv environment) (*http.Client, error) {
	if s.OAuth != nil {
		cfg, err := s.authConfig(name, vault, getenv)
		if err != nil {
			return nil, err
		}
		client, err := mcpauth.NewHTTPClient(ctx, cfg)
		if err == nil {
			client.Transport = oauthTransport{client.Transport}
		}
		return client, err
	}
	if len(s.HeadersEnv) == 0 {
		return nil, nil
	}
	origin, err := url.Parse(s.URL)
	if err != nil {
		return nil, errors.New("invalid MCP URL")
	}
	headers := make(http.Header)
	for k, env := range s.HeadersEnv {
		v := getenv(env)
		if v == "" {
			return nil, errors.New("configured HTTP header variable is unset")
		}
		headers.Set(k, v)
	}
	return &http.Client{Transport: headerTransport{origin, headers}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type oauthTransport struct{ http.RoundTripper }

func (t oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(r)
	if errors.Is(err, mcpauth.ErrLoginRequired) {
		return nil, mcpclient.ErrUnauthorized
	}
	return resp, err
}

func openMCP(servers map[string]serverSettings, vault string, warnings io.Writer, getenv environment) func(context.Context) ([]agent.MCPTool, func()) {
	return func(ctx context.Context) ([]agent.MCPTool, func()) {
		startup, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		var sessions []*mcpclient.Server
		var tools []agent.MCPTool
		for _, name := range slices.Sorted(maps.Keys(servers)) {
			if startup.Err() != nil {
				fmt.Fprintln(warnings, "bobik: MCP discovery stopped before reaching all servers")
				break
			}
			s := servers[name]
			hint := "check its URL and HTTP header configuration"
			if s.OAuth != nil {
				hint = fmt.Sprintf("run bobik mcp login %q", name)
			}
			hc, err := s.httpClient(startup, name, vault, getenv)
			if err != nil {
				fmt.Fprintf(warnings, "bobik: MCP server %q: authentication unavailable; %s\n", name, hint)
				continue
			}
			session, err := mcpclient.Discover(startup, mcpclient.Config{
				Command: s.Command, Args: s.Args, PassEnv: s.PassEnv, URL: s.URL, DisabledTools: s.DisabledTools, Timeout: time.Duration(s.TimeoutSeconds) * time.Second,
			}, hc)
			if err != nil {
				// Transport errors can include private URLs, arguments, and server output.
				reason := "discovery failed"
				if errors.Is(err, context.DeadlineExceeded) {
					reason = "discovery timed out"
				}
				if errors.Is(err, mcpclient.ErrUnauthorized) {
					reason = "authorization denied; " + hint
				}
				fmt.Fprintf(warnings, "bobik: MCP server %q: %s; skipping its tools\n", name, reason)
				continue
			}
			sessions = append(sessions, session)
			for _, tool := range session.Tools() {
				tools = append(tools, agent.MCPTool{Server: name, Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema, Call: func(ctx context.Context, args json.RawMessage) (string, error) {
					result, err := session.Call(ctx, tool.Name, args)
					if err != nil {
						if errors.Is(err, mcpclient.ErrUnauthorized) {
							fmt.Fprintf(warnings, "bobik: MCP server %q: authorization denied; %s\n", name, hint)
							return "", errors.New("MCP authorization denied")
						}
						if ctx.Err() != nil {
							return "", ctx.Err()
						}
						if errors.Is(err, context.DeadlineExceeded) {
							return "", errors.New("MCP call timed out; outcome unknown")
						}
						return "", errors.New("MCP call failed; outcome unknown")
					}
					if result.IsError {
						return "", fmt.Errorf("MCP tool reported an error: %s", result.Text)
					}
					return result.Text, nil
				}})
			}
		}
		return tools, func() {
			for _, session := range sessions {
				_ = session.Close()
			}
		}
	}
}
