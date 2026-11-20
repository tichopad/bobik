package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Source is the trusted note policy adapter. Walk synchronously visits eligible
// paths relative to the notes base, without following symlinks, and honors
// the context and callback errors. Read rechecks eligibility and file type on
// the opened object, and reads at most maxBytes, returning an error if it cannot
// return the whole file within that budget. Walk and Read should use local
// storage; the context cannot interrupt a blocked filesystem operation or Read.
type Source interface {
	Walk(context.Context, func(string, fs.FileInfo) error) error
	Read(name string, maxBytes int64) ([]byte, error)
}

// Result reports the shell's exit code, or -1 if it isn't known. A nonzero
// shell exit is not an execution error.
type Result struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

// ErrLimit means the input exceeds a command or staging budget.
var ErrLimit = errors.New("sandbox input limit exceeded")

// ErrUnavailable means the profile or runtime could not be set up.
var ErrUnavailable = errors.New("sandbox unavailable")

const (
	callTimeout = 30 * time.Second
	maxCommand  = 32 << 10
	maxOutput   = 64 << 10 // per stream
)

// Run stages the filtered notes and runs the command in the sandbox. It
// never runs a command directly, even if Bubblewrap fails.
func Run(ctx context.Context, source Source, command string) (Result, error) {
	if source == nil {
		return Result{ExitCode: -1}, errors.New("sandbox requires a notes source")
	}
	return run(ctx, source, command)
}

// Preflight runs a fixed smoke test in the production profile. It reads no notes.
func Preflight(ctx context.Context) error {
	command := "set -eo pipefail\n"
	for _, tool := range tools {
		command += tool + " --version >/dev/null\n"
	}
	command += "printf 'bobik-preflight\\n' | rg -x bobik-preflight | grep -x bobik-preflight"
	result, err := run(ctx, nil, command)
	if err != nil {
		if result.Stderr != "" {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(result.Stderr))
		}
		return err
	}
	if result.ExitCode != 0 || result.Stdout != "bobik-preflight\n" {
		return fmt.Errorf("%w: runtime smoke test exited %d: %s",
			ErrUnavailable, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func run(ctx context.Context, source Source, command string) (result Result, err error) {
	result.ExitCode = -1
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if len(command) > maxCommand {
		return result, fmt.Errorf("%w: command exceeds %d bytes", ErrLimit, maxCommand)
	}
	if strings.ContainsRune(command, 0) {
		return result, errors.New("sandbox command contains NUL")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	profile, err := loadRuntime(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		return result, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp("", "bobik-sandbox-")
	if err != nil {
		return result, fmt.Errorf("create private notes staging directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean notes staging directory: %w", cleanupErr))
		}
	}()
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return result, fmt.Errorf("resolve notes staging directory: %w", err)
	}
	if source != nil {
		if err := stage(ctx, source, absoluteDir, defaultStageBudget); err != nil {
			return result, fmt.Errorf("stage eligible notes: %w", err)
		}
	}
	return launch(ctx, profile, absoluteDir, command)
}

func launch(ctx context.Context, profile runtimeProfile, staged, command string) (Result, error) {
	result := Result{ExitCode: -1}
	status, statusWriter, err := os.Pipe()
	if err != nil {
		return result, fmt.Errorf("create sandbox status pipe: %w", err)
	}
	defer status.Close()
	defer statusWriter.Close()
	filter, err := socketFilter()
	if err != nil {
		return result, fmt.Errorf("prepare sandbox syscall filter: %w", err)
	}
	defer filter.Close()

	args := []string{
		"--as=536870912:536870912", "--cpu=30:30", "--core=0:0",
		"--nofile=128:128", "--fsize=0:0", "--", profile.bwrap,
		"--unshare-user", "--disable-userns", "--unshare-pid", "--unshare-net",
		"--unshare-ipc", "--unshare-uts", "--hostname", "bobik",
		"--cap-drop", "ALL", "--die-with-parent", "--as-pid-1",
		"--json-status-fd", "3", "--seccomp", "4",
		"--clearenv", "--setenv", "PATH", "/bin", "--setenv", "LC_ALL", "C",
		"--setenv", "HOME", "/nonexistent", "--setenv", "LD_LIBRARY_PATH", libraryPath,
		"--dev-bind", "/dev/null", "/dev/null",
		"--ro-bind", staged, "/notes",
	}
	args = append(args, profile.mounts...)
	args = append(args, "--remount-ro", "/", "--chdir", "/notes",
		"--", "/bin/bash", "--noprofile", "--norc", "-c", command)

	var stdout, stderr cappedOutput
	cmd := exec.CommandContext(ctx, profile.prlimit, args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.ExtraFiles = []*os.File{statusWriter, filter}
	// Isolate the terminal before Bubblewrap starts. Keep its setup process
	// group killable until Bash has installed Bubblewrap's parent-death
	// signal. Bash is PID 1, so Linux tears down its namespace on its exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	// Linux ties the child parent-death signal to the spawning OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		return result, executionError(ctx, err)
	}
	_ = statusWriter.Close()
	waitErr := cmd.Wait()
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.StdoutTruncated, result.StderrTruncated = stdout.truncated, stderr.truncated
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	exitCode, err := childExitStatus(status)
	if err != nil {
		return result, executionError(ctx, errors.Join(err, waitErr))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	var exitErr *exec.ExitError
	if (waitErr != nil && !errors.As(waitErr, &exitErr)) || cmd.ProcessState.ExitCode() != exitCode {
		return result, executionError(ctx, fmt.Errorf("inconsistent Bubblewrap exit status: %v", waitErr))
	}
	result.ExitCode = exitCode
	return result, nil
}

func executionError(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: Bubblewrap launch or setup failed: %w", ErrUnavailable, cause)
}

func childExitStatus(status *os.File) (int, error) {
	// This pipe is owned by Bubblewrap, never by the shell. If a broken
	// launcher leaves the descriptor open, do not wait indefinitely.
	if err := status.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		return -1, err
	}
	dec := json.NewDecoder(io.LimitReader(status, 4096))
	var exit *int
	for {
		var message struct {
			ExitCode *int `json:"exit-code"`
		}
		err := dec.Decode(&message)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return -1, err
		}
		if message.ExitCode != nil {
			if exit != nil || *message.ExitCode < 0 || *message.ExitCode > 255 {
				return -1, errors.New("invalid Bubblewrap child exit status")
			}
			exit = message.ExitCode
		}
	}
	if exit == nil {
		return -1, errors.New("Bubblewrap did not report a child exit status")
	}
	return *exit, nil
}

type cappedOutput struct {
	strings.Builder
	truncated bool
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := maxOutput - w.Len(); len(p) > remaining {
		w.truncated = true
		p = p[:remaining]
	}
	_, _ = w.Builder.Write(p)
	return n, nil
}
