package mcpauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (net.ParseIP(u.Hostname()).IsLoopback() || u.Hostname() == "localhost"))) {
		return nil, errors.New("OAuth endpoints require HTTPS (or HTTP on loopback)")
	}
	return u, nil
}

func login(ctx context.Context, cfg Config, opts LoginOptions) error {
	s, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	redirect := cfg.RedirectURL
	if redirect == "" {
		redirect = "http://127.0.0.1:0/callback"
	}
	cb, err := url.Parse(redirect)
	if err != nil || cb.Scheme != "http" || !net.ParseIP(cb.Hostname()).IsLoopback() || cb.User != nil || cb.Port() == "" || cb.RawQuery != "" || cb.Fragment != "" {
		return errors.New("callback must be an HTTP loopback IP address with a port")
	}
	ln, err := net.Listen("tcp", cb.Host)
	if err != nil {
		return err
	}
	defer ln.Close()
	cb.Host = ln.Addr().String()
	if cb.Path == "" {
		cb.Path = "/"
	}
	var c credentials
	options := &auth.AuthorizationCodeHandlerConfig{
		Client: oauthHTTP, RedirectURL: cb.String(), RequestRefreshToken: true,
		AuthorizationCodeFetcher: browserCallback(ln, cb, opts),
		NewTokenSource: func(_ context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			if !strings.EqualFold(tok.Type(), "Bearer") {
				return nil, errors.New("expected OAuth bearer token")
			}
			c = credentials{Config: *cfg, Token: *tok}
			return oauth2.StaticTokenSource(tok), nil
		},
	}
	if len(cfg.Scopes) != 0 {
		options.ScopeFilter = func([]string) []string { return cfg.Scopes }
	}
	if cfg.ClientID != "" {
		options.PreregisteredClient = &oauthex.ClientCredentials{ClientID: cfg.ClientID}
		if cfg.ClientSecret != "" {
			options.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: cfg.ClientSecret}
		}
	} else {
		if cfg.ClientSecret != "" {
			return errors.New("OAuth client secret requires a client ID")
		}
		options.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "Bobik", RedirectURIs: []string{cb.String()}, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none",
		}}
	}
	handler, err := auth.NewAuthorizationCodeHandler(options)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", cfg.ServerURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return err
	}
	// Discovery needs only the challenge, not the potentially unending SSE body.
	resp.Body.Close()
	resp.Body = http.NoBody
	if err := handler.Authorize(ctx, req, resp); err != nil {
		return safeOAuthError(err)
	}
	if c.Token.AccessToken == "" {
		return errors.New("MCP server did not authorize login")
	}
	lock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	return s.save(c)
}

func browserCallback(ln net.Listener, cb *url.URL, opts LoginOptions) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		u, err := endpoint(args.URL)
		if err != nil {
			return nil, err
		}
		state := u.Query().Get("state")
		done := make(chan *auth.AuthorizationResult, 1)
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Query()
			if r.Method != "GET" || r.Host != cb.Host || r.URL.Path != cb.Path || state == "" || p.Get("state") != state {
				http.Error(w, "Invalid OAuth callback.", http.StatusBadRequest)
				return
			}
			result := &auth.AuthorizationResult{Code: p.Get("code"), Iss: p.Get("iss"), State: state}
			if p.Has("error") {
				result.Code = ""
			}
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprintln(w, "OAuth callback received. You can close this window.")
			select {
			case done <- result:
			default:
			}
		})}
		stopped := make(chan error, 1)
		go func() { stopped <- server.Serve(ln) }()
		defer server.Close()
		open := opts.OpenBrowser
		if open == nil {
			open = func(addr string) error {
				cmd := exec.CommandContext(ctx, "xdg-open", addr)
				if err := cmd.Start(); err != nil {
					return err
				}
				go cmd.Wait()
				return nil
			}
		}
		if err := open(args.URL); err != nil {
			return nil, fmt.Errorf("open login browser: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-stopped:
			return nil, err
		case res := <-done:
			if res.Code == "" {
				return nil, errors.New("OAuth authorization denied or missing code")
			}
			return res, nil
		}
	}
}

func safeOAuthError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.Response != nil {
		return fmt.Errorf("OAuth token endpoint returned HTTP %d", re.Response.StatusCode)
	}
	return err
}

func bearer(ctx context.Context, cfg Config) (string, error) {
	s, err := openStore(cfg)
	if err != nil {
		return "", err
	}
	defer s.Close()
	lock, err := s.lock(ctx)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	c, err := s.load()
	if err != nil {
		return "", err
	}
	if c.Token.Valid() {
		return c.Token.AccessToken, nil
	}
	if c.Token.RefreshToken == "" {
		return "", ErrLoginRequired
	}
	if _, err := endpoint(c.Config.Endpoint.TokenURL); err != nil {
		return "", err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, oauthHTTP)
	tok, err := c.Config.TokenSource(ctx, &c.Token).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			return "", ErrLoginRequired
		}
		return "", safeOAuthError(err)
	}
	if !strings.EqualFold(tok.Type(), "Bearer") {
		return "", errors.New("expected OAuth bearer token")
	}
	c.Token = *tok
	return tok.AccessToken, s.save(c)
}
