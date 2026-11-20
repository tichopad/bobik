package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/tichopad/bobik/internal/agent"
	"github.com/tichopad/bobik/internal/fireworks"
	"github.com/tichopad/bobik/internal/notes"
	"github.com/tichopad/bobik/internal/sandbox"
	"github.com/tichopad/bobik/internal/watch"
)

const (
	exitOK          = 0
	exitRunError    = 1
	exitUsage       = 2
	exitInterrupted = 130
)

type app struct {
	stdout, stderr io.Writer
	getenv         environment
	cwd            func() (string, error)
	signals        <-chan os.Signal
	client         func(key string) *fireworks.Client
}

func main() {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	a := app{
		stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, cwd: os.Getwd, signals: signals,
		client: func(key string) *fireworks.Client { return &fireworks.Client{APIKey: key} },
	}
	os.Exit(a.run(os.Args[1:]))
}

func (a app) run(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(a.stdout, version)
		return exitOK
	}
	if len(args) == 1 && args[0] == "check-sandbox" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go cancelOnSignal(ctx, cancel, a.signals)
		if err := sandbox.Preflight(ctx); err != nil {
			return a.fail(exitRunError, err)
		}
		fmt.Fprintln(a.stdout, "Bash sandbox is available.")
		return exitOK
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			if len(args) > 0 && args[0] == "set-api-key" {
				fmt.Fprintln(a.stdout, "Usage: bobik set-api-key <key>")
			} else {
				fmt.Fprint(a.stdout, help)
			}
			return exitOK
		}
	}
	if len(args) > 0 && args[0] == "set-api-key" {
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return a.fail(exitUsage, errors.New("usage: bobik set-api-key <key>"))
		}
		path, err := saveKey(strings.TrimSpace(args[1]), a.getenv)
		if err != nil {
			return a.fail(exitRunError, fmt.Errorf("save API key: %w", err))
		}
		fmt.Fprintf(a.stdout, "API key saved to %s\n", path)
		return exitOK
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	o, err := parseOptions(args)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	cwd, err := a.cwd()
	if err != nil {
		return a.fail(exitRunError, err)
	}
	promptPath := o.prompt
	if !filepath.IsAbs(promptPath) {
		promptPath = filepath.Join(cwd, promptPath)
	}
	loadPrompt := func() (string, error) {
		b, err := readRegular(promptPath, promptLimit)
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("prompt file not found: %s", promptPath)
		}
		if err != nil {
			return "", fmt.Errorf("could not read prompt file: %w", err)
		}
		return string(b), nil
	}
	prompt, err := loadPrompt()
	if err != nil {
		return a.fail(exitUsage, err)
	}
	key, err := requireKey(a.getenv)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	store, err := notes.Open(o.allow, cwd)
	if err != nil {
		var fsErr *fs.PathError
		if errors.As(err, &fsErr) {
			return a.fail(exitRunError, err)
		}
		return a.fail(exitUsage, err)
	}
	defer store.Close()
	store.SetTrigger(o.trigger)
	if path, err := configPath(a.getenv); err == nil {
		// The saved key is host configuration, even if the user picks a
		// broad allowlist. Also exclude the target of a symlinked config.
		dir := filepath.Dir(path)
		for _, excluded := range []string{dir, resolvedPath(dir), resolvedPath(path)} {
			if err := store.Exclude(excluded); err != nil {
				return a.fail(exitRunError, fmt.Errorf("exclude API key configuration: %w", err))
			}
		}
	}

	client := a.client(key)
	cfg := agent.Config{
		CWD: cwd, PromptPath: o.prompt, AllowPath: o.allow, Trigger: o.trigger,
		Notes: store, Model: o.model, VisionModel: o.visionModel,
		MaxSteps: o.steps, ModelClient: client, VisionClient: client,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !o.watch {
		go cancelOnSignal(ctx, cancel, a.signals)
		cfg.Prompt = prompt
		_, err := a.runAgent(ctx, cfg, false)
		if err != nil {
			return exitRunError
		}
		return exitOK
	}
	a.stderr = &serialWriter{w: a.stderr}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- watch.Run(ctx, watch.Config{
			Root: cwd, PromptFile: promptPath, ReadPrompt: loadPrompt,
			Trigger: o.trigger, QuietPeriod: o.quietPeriod, Notes: store,
			Stdout: a.stdout, Stderr: a.stderr, Stop: stop,
			Run: func(ctx context.Context, prompt string) ([]string, error) {
				runCfg := cfg
				runCfg.Prompt = prompt
				initial, files := watch.Changes(ctx)
				runCfg.InitialRun = initial
				for _, file := range files {
					runCfg.ChangedFiles = append(runCfg.ChangedFiles, agent.ChangedFile{Path: file.Path, Deleted: file.Deleted})
				}
				runCfg.OnWrite = func(path string) { watch.RecordWrite(ctx, path) }
				return a.runAgent(ctx, runCfg, true)
			},
		})
	}()
	select {
	case err := <-done:
		return a.watchExit(err)
	case <-a.signals:
		close(stop)
	}
	select {
	case err := <-done:
		return a.watchExit(err)
	case <-a.signals:
		cancel()
		// Wait for the cancelled run to finish its audit and release its
		// notes store; no new run is scheduled after stop has closed.
		<-done
		return exitInterrupted
	}
}

func cancelOnSignal(ctx context.Context, cancel context.CancelFunc, signals <-chan os.Signal) {
	select {
	case <-signals:
		cancel()
	case <-ctx.Done():
	}
}

func (a app) runAgent(ctx context.Context, cfg agent.Config, watching bool) ([]string, error) {
	summary, err := agent.Run(ctx, cfg)
	watch.RecordLogPath(ctx, summary.LogPath)
	if err != nil {
		if !watching {
			a.fail(exitRunError, err)
		}
	} else {
		text := summary.Text
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		fmt.Fprint(a.stdout, text)
	}
	if summary.LogPath != "" {
		fmt.Fprintln(a.stderr, agent.FormatSummary(summary))
	}
	return summary.WrittenPaths, err
}

func resolvedPath(path string) string {
	path, _ = filepath.EvalSymlinks(path)
	return path
}

type serialWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *serialWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func (a app) watchExit(err error) int {
	if err != nil {
		return a.fail(exitRunError, err)
	}
	return exitOK
}

func (a app) fail(code int, err error) int {
	fmt.Fprintf(a.stderr, "bobik: %v\n", err)
	return code
}
