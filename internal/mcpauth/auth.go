// Package mcpauth authenticates remote MCP sessions using a browser and a loopback callback.
package mcpauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	ServerURL, VaultRoot string
	// Name separates named connections to the same server.
	Name                   string
	ClientID, ClientSecret string
	Scopes                 []string
	// RedirectURL can pin a loopback port for a pre-registered client.
	RedirectURL string
}

type LoginOptions struct {
	// OpenBrowser defaults to xdg-open.
	OpenBrowser func(string) error
}

var ErrLoginRequired = errors.New("MCP OAuth login required")

// Login obtains and stores new credentials. Only this operation opens a browser.
func Login(ctx context.Context, cfg Config, opts LoginOptions) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return login(ctx, cfg, opts)
}

// NewHTTPClient loads credentials and refreshes them on demand. Requests are
// restricted to the configured MCP origin; redirects are never followed.
func NewHTTPClient(ctx context.Context, cfg Config) (*http.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := openStore(cfg)
	if err != nil {
		return nil, err
	}
	_, err = store.load()
	store.Close()
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport{cfg}, CheckRedirect: noRedirect}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

var oauthHTTP = &http.Client{Transport: oauthTransport{}, Timeout: 20 * time.Second, CheckRedirect: noRedirect}

type oauthTransport struct{}

func (oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, err := endpoint(req.URL.String()); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(req)
}

type transport struct{ cfg Config }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	want, _ := url.Parse(t.cfg.ServerURL)
	if req.URL.Scheme != want.Scheme || !strings.EqualFold(req.URL.Host, want.Host) || req.URL.User != nil || (req.Host != "" && !strings.EqualFold(req.Host, want.Host)) {
		return nil, errors.New("refusing MCP credentials outside configured origin")
	}
	token, err := bearer(req.Context(), t.cfg)
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultTransport.RoundTrip(r)
}
