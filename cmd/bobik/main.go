package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/tichopad/bobik/internal/agent"
	"github.com/tichopad/bobik/internal/fireworks"
	"github.com/tichopad/bobik/internal/mcpauth"
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
	override, args, err := splitConfigOption(args)
	if err != nil {
		return a.fail(exitUsage, err)
	}
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
				fmt.Fprintln(a.stdout, "Usage: bobik set-api-key <key> [--config <path>]")
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
		path, err := a.settingsPath(override)
		if err == nil {
			err = saveKey(strings.TrimSpace(args[1]), path)
		}
		if err != nil {
			return a.fail(exitRunError, fmt.Errorf("save API key: %w", err))
		}
		fmt.Fprintf(a.stdout, "API key saved to %s\n", path)
		return exitOK
	}
	if len(args) >= 2 && args[0] == "mcp" && args[1] == "login" {
		return a.login(args[2:], override)
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
	vault := fromDirectory(cwd, o.vault)
	promptPath := fromDirectory(cwd, o.prompt)
	store, err := notes.NewVault(vault, o.rules)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	defer store.Close()
	path, err := a.settingsPath(override)
	if err != nil && (override != "" || strings.TrimSpace(a.getenv("FIREWORKS_API_KEY")) == "") {
		return a.fail(exitUsage, err)
	}
	checkPrivate := func() error { return a.checkPrivateSettings(store.Base(), path, override != "") }
	if err := checkPrivate(); err != nil {
		return a.fail(exitUsage, err)
	}
	loadPrompt := func() (string, error) { return readPrompt(store, promptPath) }
	prompt, err := loadPrompt()
	if err != nil {
		return a.fail(exitUsage, err)
	}
	var fileCfg settings
	if path != "" {
		fileCfg, err = loadConfig[settings](path, override == "")
		if err != nil {
			return a.fail(exitUsage, err)
		}
	}
	key, err := fileCfg.requireKey(a.getenv)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	client := a.client(key)
	if o.watch {
		a.stderr = &serialWriter{w: a.stderr}
	}
	cfg := agent.Config{
		PromptPath: promptPath, Trigger: o.trigger,
		Notes: store, Model: o.model, VisionModel: o.visionModel,
		MaxSteps: o.steps, ModelClient: client, VisionClient: client,
	}
	if len(fileCfg.MCPServers) > 0 {
		cfg.OpenMCP = openMCP(fileCfg.MCPServers, store.Base(), a.stderr, a.getenv)
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
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- watch.Run(ctx, watch.Config{
			PromptFile: promptPath, ReadPrompt: loadPrompt,
			Trigger: o.trigger, QuietPeriod: o.quietPeriod, Notes: store,
			Stdout: a.stdout, Stderr: a.stderr, Stop: stop,
			Run: func(ctx context.Context, prompt string) ([]string, error) {
				if err := checkPrivate(); err != nil {
					return nil, err
				}
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

func (a app) checkPrivateSettings(vault, selected string, override bool) error {
	if err := mcpauth.CheckVault(vault); err != nil {
		return err
	}
	if override {
		if path, err := a.settingsPath(""); err == nil && path != selected {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				if err := checkKeyLocation(vault, path); err != nil {
					return err
				}
			}
		}
	}
	if selected != "" {
		return checkKeyLocation(vault, selected)
	}
	return nil
}

func (a app) settingsPath(override string) (string, error) {
	path := override
	if path == "" {
		var err error
		path, err = configPath(a.getenv)
		if err != nil {
			return "", err
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	cwd, err := a.cwd()
	if err != nil {
		return "", err
	}
	return fromDirectory(cwd, path), nil
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

func fromDirectory(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
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
