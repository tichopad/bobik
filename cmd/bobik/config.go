package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tichopad/bobik/internal/notes"
	"github.com/tichopad/bobik/internal/sandbox"
	"golang.org/x/sys/unix"
)

const promptLimit = 1 << 20

func readPrompt(store *notes.Store, path string) (string, error) {
	var b []byte
	var err error
	if within(store.Base(), path) {
		b, err = store.ReadPrompt(path, promptLimit)
	} else {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if errors.Is(resolveErr, fs.ErrNotExist) {
			return "", fmt.Errorf("prompt file not found: %s", path)
		}
		if resolveErr != nil {
			return "", fmt.Errorf("resolve prompt file: %w", resolveErr)
		}
		if err := sandbox.CheckPrivatePath(store.Base(), resolved); err != nil {
			return "", fmt.Errorf("a prompt specified outside the vault must not resolve into the vault or the shell's system directories: %w", err)
		}
		b, err = readRegular(resolved, promptLimit)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("prompt file not found: %s", path)
	}
	if err != nil {
		return "", fmt.Errorf("could not read prompt file: %w", err)
	}
	return string(b), nil
}

type environment func(string) string

func configPath(getenv environment) (string, error) {
	if dir := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "bobik", "config.json"), nil
	}
	if home := strings.TrimSpace(getenv("HOME")); home != "" {
		return filepath.Join(home, ".config", "bobik", "config.json"), nil
	}
	return "", errors.New("neither HOME nor XDG_CONFIG_HOME is set")
}

type settings struct {
	APIKey     string                    `json:"apiKey"`
	MCPServers map[string]serverSettings `json:"mcpServers"`
}

func checkKeyLocation(vault, path string) error {
	resolved, err := resolveConfigPath(path)
	if err != nil {
		return fmt.Errorf("resolve saved API key location: %w", err)
	}
	for _, name := range []string{path, resolved} {
		if within(vault, name) {
			return errors.New("saved API key configuration must be outside the vault and the shell's system directories")
		}
	}
	if err := sandbox.CheckPrivatePath(vault, resolved); err != nil {
		return fmt.Errorf("saved API key configuration must be outside the vault and the shell's system directories: %w", err)
	}
	return nil
}

// The default config often doesn't exist when using an environment key.
func resolveConfigPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: missing or inaccessible symlink target", path)
	}
	parent, err := resolveConfigPath(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func (cfg settings) requireKey(getenv environment) (string, error) {
	if key := strings.TrimSpace(getenv("FIREWORKS_API_KEY")); key != "" {
		return key, nil
	}
	if key := strings.TrimSpace(cfg.APIKey); key != "" {
		return key, nil
	}
	return "", errors.New("no Fireworks API key found. Set FIREWORKS_API_KEY or run: bobik set-api-key <key>")
}

func loadConfig[T any](path string, optional bool) (T, error) {
	var cfg T
	b, err := readRegular(path, 1<<20)
	if optional && errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read bobik config: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(b)), "{") || json.Unmarshal(b, &cfg) != nil {
		return cfg, fmt.Errorf("invalid bobik config at %s: expected a JSON settings object", path)
	}
	return cfg, nil
}

func saveKey(key, path string) error {
	cfg, err := loadConfig[map[string]json.RawMessage](path, true)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = make(map[string]json.RawMessage)
	}
	for k := range cfg {
		if strings.EqualFold(k, "apiKey") {
			delete(cfg, k)
		}
	}
	cfg["apiKey"], _ = json.Marshal(key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".key-*") // 0600, including on replacement
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := errors.Join(enc.Encode(cfg), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func readRegular(path string, limit int64) ([]byte, error) {
	// Nonblocking open avoids hanging on a FIFO before checking the file type.
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("%s: exceeds %d bytes", path, limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: exceeds %d bytes", path, limit)
	}
	return b, nil
}
