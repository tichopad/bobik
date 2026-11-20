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

	"golang.org/x/sys/unix"
)

const promptLimit = 1 << 20

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

type keyConfig struct {
	APIKey string `json:"apiKey"`
}

func requireKey(getenv environment) (string, error) {
	if key := strings.TrimSpace(getenv("FIREWORKS_API_KEY")); key != "" {
		return key, nil
	}
	path, err := configPath(getenv)
	if err != nil {
		return "", err
	}
	b, err := readRegular(path, 64<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errors.New("no Fireworks API key found. Set FIREWORKS_API_KEY or run: bobik set-api-key <key>")
	}
	if err != nil {
		return "", fmt.Errorf("read bobik config: %w", err)
	}
	var cfg keyConfig
	if json.Unmarshal(b, &cfg) != nil || strings.TrimSpace(cfg.APIKey) == "" {
		// A malformed file can contain credentials; don't echo JSON parse details.
		return "", fmt.Errorf("invalid bobik config at %s: expected { \"apiKey\": \"<key>\" }", path)
	}
	return strings.TrimSpace(cfg.APIKey), nil
}

func saveKey(key string, getenv environment) (string, error) {
	path, err := configPath(getenv)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".key-*") // 0600, including on replacement
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := errors.Join(enc.Encode(keyConfig{APIKey: key}), f.Close()); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return path, nil
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
