package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Result reports the shell's exit code, or -1 if it isn't known. A nonzero
// shell exit is not an execution error.
type Result struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

// ErrLimit means the input exceeds a command or mount budget.
var ErrLimit = errors.New("sandbox input limit exceeded")

// ErrUnavailable means the profile or runtime could not be set up.
var ErrUnavailable = errors.New("sandbox unavailable")

const (
	callTimeout = 30 * time.Second
	maxCommand  = 32 << 10
	maxOutput   = 64 << 10 // per stream
)

// Run exposes the live vault at /notes with the supplied mount exceptions.
// The command never runs outside Bubblewrap.
func Run(ctx context.Context, vault string, mounts []Mount, command string) (Result, error) {
	return run(ctx, vault, mounts, command)
}

// Preflight runs a fixed smoke test against a temporary vault. It reads no notes.
func Preflight(ctx context.Context) error {
	vault, err := os.MkdirTemp("", "bobik-check-sandbox-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(vault)
	vault, err = filepath.EvalSymlinks(vault)
	if err != nil {
		return err
	}
	for _, name := range []string{"hidden", "pinned", "readonly"} {
		if err := os.Mkdir(vault+"/"+name, 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(vault+"/hidden/secret", []byte("hidden"), 0o600); err != nil {
		return err
	}
	// Optional utilities aren't deployment prerequisites.
	const command = `set -e
[[ $PWD == /notes && ! -e /proc/self && ! -e hidden/secret ]]
printf ok >live
printf ok >pinned/live
if (printf no >readonly/file) 2>/dev/null; then exit 1; fi
printf 'bobik-preflight\n'`
	result, err := Run(ctx, vault, []Mount{{"hidden", Hide}, {"pinned", Pin}, {"readonly", ReadOnly}}, command)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || result.Stdout != "bobik-preflight\n" {
		return fmt.Errorf("%w: runtime smoke test exited %d: %s",
			ErrUnavailable, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func run(ctx context.Context, vault string, mounts []Mount, command string) (Result, error) {
	result := Result{ExitCode: -1}
	if vault == "" {
		return result, errors.New("sandbox requires a vault")
	}
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
	notes, err := prepareVault(ctx, vault, mounts)
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer notes.close()
	if err := checkHostMounts(profile, notes); err != nil {
		return result, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return launch(ctx, profile, notes, command)
}

func launch(ctx context.Context, profile runtimeProfile, notes notesProfile, command string) (Result, error) {
	result := Result{ExitCode: -1}
	status, statusWriter, err := os.Pipe()
	if err != nil {
		return result, fmt.Errorf("create sandbox status pipe: %w", err)
	}
	defer status.Close()
	defer statusWriter.Close()
	filter, err := syscallFilter()
	if err != nil {
		return result, fmt.Errorf("prepare sandbox syscall filter: %w", err)
	}
	defer filter.Close()
	gate, release, err := os.Pipe()
	if err != nil {
		return result, fmt.Errorf("create sandbox startup gate: %w", err)
	}
	defer gate.Close()
	defer release.Close()

	args := []string{
		"--as=536870912:536870912", "--cpu=30:30", "--core=0:0",
		"--nofile=128:128", "--fsize=33554432:33554432", "--", profile.bwrap,
		"--unshare-user", "--disable-userns", "--unshare-pid", "--unshare-net",
		"--unshare-ipc", "--unshare-uts", "--hostname", "bobik",
		"--cap-drop", "ALL", "--die-with-parent", "--as-pid-1",
		"--json-status-fd", "3", "--seccomp", "4", "--block-fd", "5",
		"--clearenv", "--setenv", "PATH", "/usr/bin:/bin", "--setenv", "LC_ALL", "C",
		"--setenv", "HOME", "/nonexistent",
		"--dev-bind", "/dev/null", "/dev/null",
	}
	args = append(args, profile.args...)
	args = append(args, notes.args...)
	args = append(args, "--remount-ro", "/", "--chdir", "/notes",
		"--", profile.bash, "--noprofile", "--norc", "-c", bootstrap)

	var stdout, stderr cappedOutput
	cmd := exec.Command(profile.prlimit, args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Dir = "/"
	cmd.Stdin = gate
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.ExtraFiles = append([]*os.File{statusWriter, filter, gate}, notes.files...)
	// Isolate the terminal before Bubblewrap starts. Keep its setup process
	// group killable until Bash has installed Bubblewrap's parent-death
	// signal. Bash is PID 1, so Linux tears down its namespace on its exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = time.Second
	// Linux ties the child parent-death signal to the spawning OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := cmd.Start(); err != nil {
		return result, executionError(ctx, err)
	}
	_ = statusWriter.Close()
	_ = gate.Close()
	decoder := json.NewDecoder(io.LimitReader(status, 4096))
	pid, setupErr := sandboxPID(ctx, status, decoder)
	if setupErr == nil {
		setupErr = verifySandbox(ctx, pid, profile, notes)
	}
	if setupErr == nil {
		setupErr = ctx.Err()
	}
	if setupErr == nil {
		setupErr = sendCommand(ctx, release, command)
		_ = release.Close()
	}
	if setupErr != nil {
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	}
	waitErr := waitForGroup(ctx, cmd)
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.StdoutTruncated, result.StderrTruncated = stdout.truncated, stderr.truncated
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if setupErr != nil {
		if diagnostic := strings.TrimSpace(result.Stderr); diagnostic != "" {
			setupErr = fmt.Errorf("%w: %s", setupErr, diagnostic)
		}
		return result, executionError(ctx, setupErr)
	}
	exitCode, err := childExitStatus(status, decoder)
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

// The block fd also unblocks on EOF. Require a complete NUL-terminated
// command from the launcher. Keep its bytes out of process arguments.
const bootstrap = `IFS= read -r -d '' _bobik_command || exit 125
eval -- $'unset _bobik_command\n'"$_bobik_command"`

func sendCommand(ctx context.Context, release *os.File, command string) error {
	deadline, _ := ctx.Deadline()
	if err := release.SetWriteDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = release.SetWriteDeadline(time.Now()) })
	defer stop()
	// Bubblewrap takes the first byte through --block-fd; Bash reads the rest
	// through stdin, which shares the same pipe.
	_, err := release.WriteString("!" + command + "\x00")
	return err
}

func waitForGroup(ctx context.Context, cmd *exec.Cmd) error {
	killGroup := func() {
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	}
	killed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		killGroup()
		close(killed)
	})

	// Observe exit without reaping the leader. While the PID is still
	// reserved, stop the cancellation hook and clean up any leftover setup
	// processes. A delayed group kill must never hit a recycled ID.
	var info unix.Siginfo
	var observeErr error
	for {
		observeErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(observeErr, unix.EINTR) {
			break
		}
	}
	if !stop() {
		<-killed
	}
	if observeErr == nil {
		killGroup()
	}
	return errors.Join(observeErr, cmd.Wait())
}

func executionError(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: Bubblewrap launch or setup failed: %w", ErrUnavailable, cause)
}

func sandboxPID(ctx context.Context, status *os.File, dec *json.Decoder) (int, error) {
	deadline, _ := ctx.Deadline()
	if err := status.SetReadDeadline(deadline); err != nil {
		return 0, err
	}
	stop := context.AfterFunc(ctx, func() { _ = status.SetReadDeadline(time.Now()) })
	defer stop()
	var message struct {
		PID int `json:"child-pid"`
	}
	if err := dec.Decode(&message); err != nil {
		return 0, fmt.Errorf("read Bubblewrap child PID: %w", err)
	}
	if message.PID <= 0 {
		return 0, errors.New("Bubblewrap did not report a valid child PID")
	}
	return message.PID, nil
}

func childExitStatus(status *os.File, dec *json.Decoder) (int, error) {
	// This pipe is owned by Bubblewrap, never by the shell. If a broken
	// launcher leaves the descriptor open, do not wait indefinitely.
	if err := status.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		return -1, err
	}
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
