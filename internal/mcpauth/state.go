package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tichopad/bobik/internal/sandbox"
	"golang.org/x/oauth2"
	"golang.org/x/sys/unix"
)

type credentials struct {
	Config oauth2.Config
	Token  oauth2.Token
}

type store struct {
	*os.Root
	name string
}

func stateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "bobik", "mcpauth"), nil
}

func contains(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

func checkLocation(path, vault string) error {
	if vault == "" {
		// Login has no vault; check the permanent sandbox runtime binds.
		vault = "/usr/bin"
	}
	vault, err := filepath.Abs(vault)
	if err != nil {
		return err
	}
	vault, err = filepath.EvalSymlinks(vault)
	if err != nil {
		return err
	}
	if contains(path, vault) {
		return errors.New("OAuth state overlaps the vault")
	}
	return sandbox.CheckPrivatePath(vault, path)
}

// CheckVault rejects exposing existing OAuth state to tools, even when a run
// has no OAuth servers configured. It does not create any state.
func CheckVault(vaultRoot string) error {
	path, err := stateDir()
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkLocation(resolved, vaultRoot); err != nil {
		return err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return errors.New("unexpected OAuth state entry")
		}
		if err := checkLocation(filepath.Join(resolved, entry.Name()), vaultRoot); err != nil {
			return err
		}
	}
	return nil
}

func openStore(cfg Config) (*store, error) {
	if _, err := endpoint(cfg.ServerURL); err != nil {
		return nil, err
	}
	path, err := stateDir()
	if err != nil {
		return nil, err
	}
	if err := checkLocation(path, cfg.VaultRoot); err != nil {
		return nil, err
	}
	// Walk without following symlinks, including while creating missing parents.
	dir, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	defer func() { dir.Close() }()
	for part := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		fd := int(dir.Fd())
		n, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(fd, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, err
			}
			n, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			return nil, err
		}
		dir.Close()
		dir = os.NewFile(uintptr(n), part)
		fi, err := dir.Stat()
		if err != nil {
			return nil, err
		}
		uid := fi.Sys().(*syscall.Stat_t).Uid
		if (uid != 0 && uid != uint32(os.Geteuid())) || (fi.Mode().Perm()&0022 != 0 && fi.Mode()&os.ModeSticky == 0) {
			return nil, fmt.Errorf("untrusted OAuth state ancestor %q", part)
		}
	}
	if err := private(dir); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
	if err != nil {
		return nil, err
	}
	key, _ := json.Marshal([]any{cfg.Name, cfg.ServerURL, cfg.ClientID, cfg.Scopes})
	return &store{root, fmt.Sprintf("%x", sha256.Sum256(key))}, nil
}

func private(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0077 != 0 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("OAuth state must be owner-only")
	}
	return nil
}

func (s *store) load() (c credentials, err error) {
	f, err := s.OpenFile(s.name+".json", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return c, ErrLoginRequired
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err := private(f); err != nil {
		return c, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Sys().(*syscall.Stat_t).Nlink != 1 {
		return c, errors.New("invalid OAuth state file")
	}
	err = json.NewDecoder(io.LimitReader(f, 2<<20)).Decode(&c)
	if err != nil || c.Token.AccessToken == "" {
		return c, ErrLoginRequired
	}
	return c, nil
}

func (s *store) save(c credentials) error {
	tmp := ".tmp-" + rand.Text()
	f, err := s.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer s.Remove(tmp)
	err = json.NewEncoder(f).Encode(c)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return s.Rename(tmp, s.name+".json")
}

func (s *store) lock(ctx context.Context) (*os.File, error) {
	f, err := s.OpenFile(s.name+".lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
