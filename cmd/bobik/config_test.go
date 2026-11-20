package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConfigPrecedenceAndPrivateReplacement(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOME": filepath.Join(dir, "home"), "XDG_CONFIG_HOME": filepath.Join(dir, "xdg")}
	get := func(k string) string { return env[k] }
	path, err := saveKey("first", get)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "xdg", "bobik", "config.json"); path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := saveKey("second", get); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("insecure config file: %v, %v", st, err)
	}
	if got, err := requireKey(get); got != "second" || err != nil {
		t.Fatalf("unexpected file key: %q, %v", got, err)
	}
	env["FIREWORKS_API_KEY"] = "  env-key  "
	if got, err := requireKey(get); got != "env-key" || err != nil {
		t.Fatalf("unexpected environment key: %q, %v", got, err)
	}
	delete(env, "XDG_CONFIG_HOME")
	if got, err := configPath(get); got != filepath.Join(dir, "home", ".config", "bobik", "config.json") || err != nil {
		t.Fatalf("wrong HOME fallback: %q, %v", got, err)
	}
}

func TestMalformedConfigDoesNotEchoSecret(t *testing.T) {
	get := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return t.TempDir()
		}
		return ""
	}
	dir := get("XDG_CONFIG_HOME")
	env := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return dir
		}
		return ""
	}
	path, err := saveKey("irrelevant", env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"apiKey":"secret"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := requireKey(env); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected sanitized error, got %v", err)
	}
}

func TestReadRegularRejectsFIFOAndOversize(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(fifo, 10); err == nil {
		t.Fatal("accepted FIFO")
	}
	file := filepath.Join(dir, "prompt")
	if err := os.WriteFile(file, []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(file, 5); err == nil {
		t.Fatal("accepted oversized prompt")
	}
}

func TestRunOptions(t *testing.T) {
	base := []string{"--prompt", "prompt.md", "--allow-path", "notes/**/*.md"}
	o, err := parseOptions(append(base, "--model=id", "--watch-delay", "0.5", "--max-steps=2"))
	if err != nil || o.model != "id" || o.visionModel != "id" || o.quietPeriod != 500*time.Millisecond || o.steps != 2 {
		t.Fatalf("options %+v: %v", o, err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--deny", "rm"}, "was removed"},
		{[]string{"--deny=rm"}, "was removed"},
		{[]string{"--trigger= "}, "--trigger"},
		{[]string{"--watch-delay=NaN"}, "--watch-delay"},
		{[]string{"--watch-delay=-1"}, "--watch-delay"},
		{[]string{"--watch-delay=2147484"}, "--watch-delay"},
		{[]string{"--max-steps=0"}, "--max-steps"},
		{[]string{"--max-steps=1.1"}, "--max-steps"},
		{[]string{"--vision-model="}, "--vision-model"},
		{[]string{"--model", "--watch"}, "--model"},
		{[]string{"--typo"}, "not defined"},
	} {
		if _, err := parseOptions(append(base, tc.args...)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: expected %q, got %v", tc.args, tc.want, err)
		}
	}
}
