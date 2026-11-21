package mcpauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func TestBrowserLoginAndRefresh(t *testing.T) {
	for _, manual := range []bool{true, false} {
		t.Run(fmt.Sprintf("manual=%v", manual), func(t *testing.T) {
			stateHome := t.TempDir()
			if err := os.Chmod(stateHome, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_STATE_HOME", stateHome)
			var refresh, registrations atomic.Int32
			var challenge atomic.Value
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			defer srv.Close()
			cfg := Config{ServerURL: srv.URL + "/mcp", VaultRoot: t.TempDir()}
			if manual {
				cfg.ClientID, cfg.ClientSecret = "manual", "secret"
			}
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer renewed" {
					w.WriteHeader(204)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="MCP", resource_metadata="`+srv.URL+`/resource", scope="read"`)
				w.WriteHeader(401)
			})
			mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"resource": cfg.ServerURL, "authorization_servers": []string{srv.URL + "/issuer"}})
			})
			mux.HandleFunc("/.well-known/oauth-authorization-server/issuer", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(oauthex.AuthServerMeta{Issuer: srv.URL + "/issuer", AuthorizationEndpoint: srv.URL + "/authorize", TokenEndpoint: srv.URL + "/token", RegistrationEndpoint: srv.URL + "/register", ResponseTypesSupported: []string{"code"}})
			})
			mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
				registrations.Add(1)
				var reg struct {
					Redirects []string `json:"redirect_uris"`
					Method    string   `json:"token_endpoint_auth_method"`
				}
				if json.NewDecoder(r.Body).Decode(&reg) != nil || len(reg.Redirects) != 1 || reg.Method != "none" {
					t.Error("invalid registration")
				}
				fmt.Fprint(w, `{"client_id":"auto","token_endpoint_auth_method":"none"}`)
			})
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if manual {
					id, secret, ok := r.BasicAuth()
					if !ok || id != cfg.ClientID || secret != cfg.ClientSecret {
						t.Error("wrong client authentication")
					}
				} else if r.PostFormValue("client_id") != "auto" {
					t.Error("missing registered ID")
				}
				if r.PostFormValue("grant_type") != "refresh_token" && r.PostFormValue("resource") != cfg.ServerURL {
					t.Error("wrong resource")
				}
				if r.PostFormValue("grant_type") == "refresh_token" {
					if r.PostFormValue("refresh_token") != "refresh" {
						t.Error("wrong refresh token")
					}
					refresh.Add(1)
					fmt.Fprint(w, `{"token_type":"Bearer","access_token":"renewed","refresh_token":"rotated","expires_in":3600}`)
					return
				}
				v := r.PostFormValue("code_verifier")
				hash := sha256.Sum256([]byte(v))
				if len(v) < 43 || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge.Load() || r.PostFormValue("code") != "ok" {
					t.Error("invalid PKCE exchange")
				}
				fmt.Fprint(w, `{"token_type":"Bearer","access_token":"old","refresh_token":"refresh","expires_in":1}`)
			})
			ctx := context.Background()
			if _, err := NewHTTPClient(ctx, cfg); !errors.Is(err, ErrLoginRequired) {
				t.Fatalf("before login: %v", err)
			}
			err := Login(ctx, cfg, LoginOptions{OpenBrowser: func(addr string) error {
				u, err := url.Parse(addr)
				if err != nil {
					return err
				}
				q := u.Query()
				if q.Get("scope") != "read" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != cfg.ServerURL {
					return errors.New("bad authorization URL")
				}
				challenge.Store(q.Get("code_challenge"))
				for _, state := range []string{"invalid", q.Get("state")} {
					r, err := http.Get(q.Get("redirect_uri") + "?code=ok&state=" + state)
					if err != nil {
						return err
					}
					r.Body.Close()
					if (state == "invalid") != (r.StatusCode == 400) {
						return errors.New("callback state validation failed")
					}
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			cl, err := NewHTTPClient(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				resp, err := cl.Get(cfg.ServerURL)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != 204 {
					t.Fatalf("resource status %d", resp.StatusCode)
				}
			}
			mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer renewed" {
					t.Error("missing credentials on same-origin messages endpoint")
				}
			})
			resp, err := cl.Post(srv.URL+"/messages?session=123", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if _, err := cl.Get("https://unrelated.invalid/messages"); err == nil {
				t.Fatal("sent credentials outside origin")
			}
			if refresh.Load() != 1 || (registrations.Load() == 1) == manual {
				t.Fatal("unexpected registration or refresh count")
			}
			s, err := openStore(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			c, err := s.load()
			if err != nil || c.Token.RefreshToken != "rotated" {
				t.Fatal("refresh rotation not persisted", err)
			}
			for _, name := range []string{".", s.name + ".json", s.name + ".lock"} {
				info, err := s.Stat(name)
				if err != nil || info.Mode().Perm()&0077 != 0 {
					t.Fatalf("insecure %s: %v", name, err)
				}
			}
		})
	}
}

func TestUnsafeStateLocations(t *testing.T) {
	vault := t.TempDir()
	cfg := Config{ServerURL: "https://example.com/mcp", VaultRoot: vault}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(vault, link); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{vault, filepath.Join(vault, "state"), link} {
		t.Setenv("XDG_STATE_HOME", base)
		if _, err := openStore(cfg); err == nil {
			t.Fatalf("accepted unsafe state location %q", base)
		}
	}
	if files, err := os.ReadDir(vault); err != nil || len(files) != 0 {
		t.Fatal("modified vault", err)
	}
}

func TestCheckVaultAfterStandaloneLogin(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", base)
	s, err := openStore(Config{ServerURL: "https://example.org/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.save(credentials{}); err != nil {
		t.Fatal(err)
	}
	if err := CheckVault(base); err == nil {
		t.Fatal("exposed state accepted")
	}
	vault := t.TempDir()
	if err := CheckVault(vault); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(base, "bobik", "mcpauth", s.name+".json")
	if err := os.Link(src, filepath.Join(vault, "note.md")); err != nil {
		t.Fatal(err)
	}
	if err := CheckVault(vault); err == nil {
		t.Fatal("exposed hardlink accepted")
	}
}

func TestLockHonorsCancellation(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", base)
	s, err := openStore(Config{ServerURL: "https://example.org/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lock, err := s.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock: %v", err)
	}
}
