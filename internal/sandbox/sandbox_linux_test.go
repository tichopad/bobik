package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunChecksInputBeforeSetup(t *testing.T) {
	vault := t.TempDir()
	if _, err := Run(t.Context(), vault, nil, strings.Repeat(" ", maxCommand+1)); !errors.Is(err, ErrLimit) {
		t.Fatalf("oversize command: %v", err)
	}
	if _, err := Run(t.Context(), vault, nil, "printf '\x00'"); err == nil {
		t.Fatal("accepted a NUL in command")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Run(ctx, vault, nil, "printf ignored"); !errors.Is(err, context.Canceled) {
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
			result, err := launch(t.Context(), runtimeProfile{prlimit: tc.path}, notesProfile{}, "printf wrong")
			if !errors.Is(err, ErrUnavailable) || result.ExitCode != -1 || result.Stdout != "" {
				t.Fatalf("launch = %+v, %v; want no shell run and ErrUnavailable", result, err)
			}
		})
	}
}

func TestLaunchIncludesSetupStderrInError(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), "launcher")
	const diagnostic = "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted"
	script := "#!/bin/sh\nprintf '%s\\n' '" + diagnostic + "' >&2\nexit 1\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := launch(t.Context(), runtimeProfile{prlimit: launcher}, notesProfile{}, "printf wrong")
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), diagnostic) || result.Stdout != "" {
		t.Fatalf("launch = %+v, %v; want setup diagnostic and no shell run", result, err)
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

func TestBootstrapKeepsTheCommandOutOfArguments(t *testing.T) {
	command := "  # a leading comment\n" + `
	[[ ! -v _bobik_command && $# == 0 ]] || exit 99
	if IFS= read -r extra; then exit 98; fi
	printf '%s\n' 'a "quote" and a backslash \'
	printf '%s' '  last line  '`
	bash, err := systemExecutable("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", bootstrap)
	cmd.Stdin = strings.NewReader(command + "\x00")
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "a \"quote\" and a backslash \\\n  last line  " {
		t.Fatalf("bootstrap = %q, %v", out, err)
	}
	cmd = exec.Command(bash, "--noprofile", "--norc", "-c", bootstrap)
	cmd.Stdin = strings.NewReader("printf leaked")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || len(out) != 0 {
		t.Fatalf("incomplete command escaped bootstrap = %q, %v", out, err)
	}
}

// Exercise the process-group wait even on hosts that prohibit Bubblewrap.
func TestWaitForGroupCancelsDescendants(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.Command("/bin/bash", "-c", `/bin/sleep 30 & printf '%s\n' "$!"; wait`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	cancel()
	var killed *exec.ExitError
	if err := waitForGroup(ctx, cmd); !errors.As(err, &killed) {
		t.Fatalf("waitForGroup = %v, want runner killed", err)
	}
	if err != nil || strings.TrimSpace(line) == "" {
		t.Fatalf("descendant pid = %q, %v", line, err)
	}
	pid := strings.TrimSpace(line)
	deadline := time.Now().Add(time.Second)
	for {
		cmdline, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if errors.Is(err, os.ErrNotExist) || err == nil && len(cmdline) == 0 {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("descendant %s survived cancellation: %q, %v", pid, cmdline, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Set BOBIK_REQUIRE_SANDBOX=1 on the deployment verification host to make
// skips a failure, including namespace restrictions that only appear under the
// intended service user, AppArmor policy, or container.
func TestBubblewrap(t *testing.T) {
	vault := t.TempDir()
	if err := Preflight(t.Context()); err != nil {
		result, runErr := Run(t.Context(), vault, nil, "printf wrong")
		if !errors.Is(runErr, ErrUnavailable) || result.ExitCode != -1 || result.Stdout != "" {
			t.Fatalf("failed production profile did not fail closed: result=%+v err=%v; preflight: %v", result, runErr, err)
		}
		if os.Getenv("BOBIK_REQUIRE_SANDBOX") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	t.Run("live vault and mount exceptions", func(t *testing.T) {
		for _, tool := range []string{"cat", "ln", "mkdir", "mkfifo", "mv", "rm", "rmdir"} {
			if _, err := systemExecutable(tool); err != nil {
				t.Skipf("optional test command %s unavailable: %v", tool, err)
			}
		}
		t.Setenv("BOBIK_SANDBOX_TEST_SECRET", "oracle")
		host := filepath.Join(t.TempDir(), "excluded.md")
		if err := os.WriteFile(host, []byte("needle in excluded host file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"project", "project/readonly", "project/hidden", "project/pinned", ".agent-bobik"} {
			if err := os.Mkdir(filepath.Join(vault, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"project/note.md", "project/readonly/keep", "project/hidden/key", ".agent-bobik/state", "delete.md"} {
			if err := os.WriteFile(filepath.Join(vault, name), []byte("needle in note\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(host, filepath.Join(vault, "project/hidden/link.md")); err != nil {
			t.Fatal(err)
		}
		rules := []Mount{{".agent-bobik", Hide}, {"project/hidden", Hide}, {"project/pinned", Pin}, {"project/readonly", ReadOnly}}
		command := fmt.Sprintf(`
			set -eo pipefail
			[[ $PWD == /notes && ! -v BOBIK_SANDBOX_TEST_SECRET ]]
			[[ ! -e %[1]q && ! -e /etc/passwd && ! -e /proc/self ]]
			[[ ! -e project/hidden/link.md && ! -e project/hidden/key && ! -e .agent-bobik/state ]]
			if mv /notes /moved 2>/dev/null; then exit 95; fi
			if (printf nope >>project/readonly/keep) 2>/dev/null; then exit 90; fi
			if (mv project moved) 2>/dev/null; then exit 91; fi
			if (mv project/pinned project/moved || rmdir project/pinned) 2>/dev/null; then exit 92; fi
			if (printf nope >/usr/lib/bobik-file) 2>/dev/null; then exit 93; fi
			if (ln project/note.md project/alias || ln -s %[1]q symlink || mkfifo fifo) 2>/dev/null; then exit 94; fi
			rm delete.md
			mkdir new-dir
			printf new >new-dir/new.md
			printf live >project/pinned/note.md
			cat project/pinned/note.md
			printf diagnostic >&2`, host)
		result, err := Run(t.Context(), vault, rules, command)
		if err != nil || result.ExitCode != 0 || result.Stdout != "live" || result.Stderr != "diagnostic" {
			t.Fatalf("Run = %+v, %v", result, err)
		}
		reopened, err := os.ReadFile(filepath.Join(vault, "new-dir", "new.md"))
		if err != nil || string(reopened) != "new" {
			t.Fatalf("live write: %q, %v", reopened, err)
		}
		if _, err := os.Stat(filepath.Join(vault, "delete.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("allowed file was not deleted: %v", err)
		}
	})
	t.Run("exit one is a result", func(t *testing.T) {
		got, err := Run(t.Context(), t.TempDir(), nil, "exit 1")
		if err != nil || got.ExitCode != 1 || got.Stdout != "" || got.Stderr != "" {
			t.Fatalf("Run = %+v, %v", got, err)
		}
	})
	t.Run("socket denied", func(t *testing.T) {
		got, err := Run(t.Context(), t.TempDir(), nil, "printf '' >/dev/tcp/127.0.0.1/12345")
		if err != nil || got.ExitCode == 0 || !strings.Contains(got.Stderr, "Operation not permitted") {
			t.Fatalf("local socket was not denied: %+v, %v", got, err)
		}
	})
	t.Run("both streams drain", func(t *testing.T) {
		command := fmt.Sprintf(`printf '%%0%dd' 0; printf '%%0%dd' 0 >&2; exit 4`, maxOutput+1, maxOutput+2)
		got, err := Run(t.Context(), t.TempDir(), nil, command)
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
			_, err := Run(ctx, t.TempDir(), nil, command)
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
