package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestRunChecksInputBeforeWalking(t *testing.T) {
	source := sourceStub{}
	if _, err := Run(t.Context(), source, strings.Repeat(" ", maxCommand+1)); !errors.Is(err, ErrLimit) {
		t.Fatalf("oversize command: %v", err)
	}
	if _, err := Run(t.Context(), source, "printf '\x00'"); err == nil {
		t.Fatal("accepted a NUL in command")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Run(ctx, source, "printf ignored"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Run: %v", err)
	}
}

func TestLaunchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		path string
	}{
		{"missing launcher", filepath.Join(dir, "missing")},
		{"launcher fails", "/usr/bin/false"},
		{"launcher succeeds without running shell", "/usr/bin/true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := launch(t.Context(), runtimeProfile{prlimit: tc.path}, dir, "printf wrong")
			if !errors.Is(err, ErrUnavailable) || result.ExitCode != -1 || result.Stdout != "" {
				t.Fatalf("launch = %+v, %v; want no shell run and ErrUnavailable", result, err)
			}
		})
	}
}

func TestCappedOutput(t *testing.T) {
	var out cappedOutput
	for _, size := range []int{maxOutput, 1, 4096} {
		if n, err := out.Write([]byte(strings.Repeat("x", size))); n != size || err != nil {
			t.Fatalf("write = %d, %v; want %d, nil", n, err, size)
		}
		if out.Len() != maxOutput || out.truncated != (size != maxOutput) {
			t.Fatalf("length = %d, truncated = %t after write of %d", out.Len(), out.truncated, size)
		}
	}
}

// Set BOBIK_REQUIRE_SANDBOX=1 on the deployment verification host to make
// skips a failure, including namespace restrictions that only appear under the
// intended service user, AppArmor policy, or container.
func TestBubblewrap(t *testing.T) {
	if err := Preflight(t.Context()); err != nil {
		result, runErr := Run(t.Context(), &fixtureSource{}, "printf wrong")
		if !errors.Is(runErr, ErrUnavailable) || result.ExitCode != -1 || result.Stdout != "" {
			t.Fatalf("failed production profile did not fail closed: result=%+v err=%v; preflight: %v", result, runErr, err)
		}
		if os.Getenv("BOBIK_REQUIRE_SANDBOX") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	t.Run("read-only search", func(t *testing.T) {
		t.Setenv("BOBIK_SANDBOX_TEST_SECRET", "oracle")
		temp := t.TempDir()
		t.Setenv("TMPDIR", temp)
		host := filepath.Join(temp, "excluded.md")
		if err := os.WriteFile(host, []byte("needle in excluded host file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		source := &fixtureSource{files: fstest.MapFS{
			"project/note.md":   {Data: []byte("needle in note\n")},
			"project/other.md":  {Data: []byte("ordinary text\n")},
			"project/link.md":   {Mode: os.ModeSymlink, Data: []byte(host)},
			"project/socket.md": {Mode: os.ModeSocket},
		}}
		command := fmt.Sprintf(`
			set -eo pipefail
			[[ $PWD == /notes && ! -v BOBIK_SANDBOX_TEST_SECRET ]]
			[[ ! -e %[1]q && ! -e /etc/passwd && ! -e /proc/self && ! -e /bin/sh ]]
			[[ ! -e project/link.md && ! -e project/socket.md ]]
			if (printf nope >>project/note.md) 2>/dev/null; then exit 90; fi
			if (printf nope >new.md) 2>/dev/null; then exit 91; fi
			if (printf nope >/runtime/new) 2>/dev/null; then exit 92; fi
			[[ $(rg -l needle) == project/note.md ]]
			rg --color never --no-heading --with-filename needle | grep needle
			printf diagnostic >&2`, host)
		result, err := Run(t.Context(), source, command)
		if err != nil || result.ExitCode != 0 || result.Stdout != "project/note.md:needle in note\n" || result.Stderr != "diagnostic" {
			t.Fatalf("Run = %+v, %v", result, err)
		}
		entries, err := os.ReadDir(temp)
		if err != nil || len(entries) != 1 || entries[0].Name() != "excluded.md" {
			t.Fatalf("staging directory was not cleaned up: %v, %v", entries, err)
		}
	})
	t.Run("exit one is a result", func(t *testing.T) {
		got, err := Run(t.Context(), &fixtureSource{}, "rg -F nonmatch <<< match")
		if err != nil || got.ExitCode != 1 || got.Stdout != "" || got.Stderr != "" {
			t.Fatalf("Run = %+v, %v", got, err)
		}
	})
	t.Run("socket denied", func(t *testing.T) {
		got, err := Run(t.Context(), &fixtureSource{}, "printf '' >/dev/tcp/127.0.0.1/12345")
		if err != nil || got.ExitCode == 0 || !strings.Contains(got.Stderr, "Operation not permitted") {
			t.Fatalf("local socket was not denied: %+v, %v", got, err)
		}
	})
	t.Run("both streams drain", func(t *testing.T) {
		command := fmt.Sprintf(`printf '%%0%dd' 0; printf '%%0%dd' 0 >&2; exit 4`, maxOutput+1, maxOutput+2)
		got, err := Run(t.Context(), &fixtureSource{}, command)
		if err != nil || got.ExitCode != 4 || len(got.Stdout) != maxOutput || len(got.Stderr) != maxOutput ||
			!got.StdoutTruncated || !got.StderrTruncated {
			t.Fatalf("output: exit=%d stdout=%d/%t stderr=%d/%t err=%v",
				got.ExitCode, len(got.Stdout), got.StdoutTruncated, len(got.Stderr), got.StderrTruncated, err)
		}
	})
	t.Run("cancellation kills descendants", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		marker := fmt.Sprintf("bobik-test-%d-%d", os.Getpid(), time.Now().UnixNano())
		command := "(/bin/bash -c 'exec -a \"$1\" /bin/bash -c \"while :; do :; done\"' bash " + marker + ") & wait"
		done := make(chan error, 1)
		go func() {
			_, err := Run(ctx, &fixtureSource{}, command)
			done <- err
		}()
		deadline := time.Now().Add(3 * time.Second)
		pid := findProcess(marker)
		for pid == "" && time.Now().Before(deadline) {
			select {
			case err := <-done:
				t.Fatalf("Run exited before descendant appeared: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			pid = findProcess(marker)
		}
		if pid == "" {
			t.Fatal("sandbox descendant was not observable")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error = %v, want context.Canceled", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return after cancellation")
		}
		if got := findProcess(marker); got != "" {
			t.Fatalf("sandbox descendant %s survived cancellation", got)
		}
	})
}

func findProcess(argv0 string) string {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name()[0] < '0' || entry.Name()[0] > '9' {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if arg, _, ok := strings.Cut(string(data), "\x00"); err == nil && ok && arg == argv0 {
			return entry.Name()
		}
	}
	return ""
}
