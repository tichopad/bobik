// Package agent runs one bounded notes-agent invocation.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/tichopad/bobik/internal/audit"
	"github.com/tichopad/bobik/internal/fireworks"
	"github.com/tichopad/bobik/internal/notes"
)

const (
	MaxSteps   = 25
	RunTimeout = 5 * time.Minute
	MaxHistory = 8 << 20 // bytes of message data, before JSON escaping
)

var (
	ErrStepLimit    = errors.New("step limit reached")
	ErrHistoryLimit = errors.New("conversation exceeds the 8 MiB history limit")
	ErrModel        = errors.New("model request failed")
)

type ModelClient interface {
	Complete(context.Context, fireworks.Request) (fireworks.Response, error)
}

type VisionClient interface {
	ViewMedia(context.Context, interface {
		Read(string, int64) ([]byte, error)
	}, string, string, string) (string, fireworks.Usage, error)
}

type Config struct {
	// CWD must be the same directory passed when opening Notes.
	CWD                           string
	PromptPath, Prompt, AllowPath string
	Model, VisionModel, LogPath   string
	Trigger                       string
	ChangedFiles                  []ChangedFile
	InitialRun                    bool
	Now                           time.Time
	MaxSteps                      int
	Timeout                       time.Duration
	Notes                         *notes.Store
	ModelClient                   ModelClient
	VisionClient                  VisionClient
	Verbose                       io.Writer
	// OnWrite receives the absolute CWD-based path after a successful mutation,
	// including deletion, before writing the tool result to the audit.
	OnWrite func(string)
}

type ChangedFile struct {
	Path    string
	Deleted bool
}

type Summary struct {
	Text         string
	Steps        int
	FilesWritten int
	// WrittenPaths is sorted, deduplicated, and relative to CWD.
	WrittenPaths    []string
	Usage           fireworks.Usage
	DurationMs      int64
	LogPath         string
	LogRelativePath string
	ModelID         string
}

// Run returns whatever statistics are available even when the run fails.
func Run(parent context.Context, cfg Config) (summary Summary, runErr error) {
	start := time.Now()
	if cfg.Notes == nil || cfg.ModelClient == nil {
		return summary, errors.New("notes store and model client are required")
	}
	if cfg.CWD == "" {
		return summary, errors.New("agent working directory is required")
	}
	var err error
	cfg.CWD, err = filepath.Abs(cfg.CWD)
	if err != nil {
		return summary, fmt.Errorf("resolve working directory: %w", err)
	}
	if cfg.Now.IsZero() {
		cfg.Now = start.In(hostTimeZone())
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = MaxSteps
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = RunTimeout
	}
	if cfg.VisionModel == "" {
		cfg.VisionModel = cfg.Model
	}
	if cfg.VisionClient == nil {
		cfg.VisionClient, _ = cfg.ModelClient.(VisionClient)
	}
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()

	log, err := audit.New(cfg.CWD, cfg.LogPath, cfg.Now)
	if err != nil {
		return summary, fmt.Errorf("could not create the audit log: %w", err)
	}
	summary.ModelID = cfg.Model
	summary.LogPath = log.Path
	summary.LogRelativePath = log.RelativePath
	state := runState{cfg: cfg, log: log, written: make(map[string]struct{})}
	defer func() {
		summary.DurationMs = time.Since(start).Milliseconds()
		summary.WrittenPaths = make([]string, 0, len(state.written))
		for path := range state.written {
			summary.WrittenPaths = append(summary.WrittenPaths, path)
		}
		slices.Sort(summary.WrittenPaths)
		summary.FilesWritten = len(summary.WrittenPaths)
		summary.Usage = addUsage(state.outerUsage, state.visionUsage)
		end := map[string]any{
			"status": "success", "durationMs": summary.DurationMs, "steps": summary.Steps,
			"filesWritten": summary.WrittenPaths, "totalUsage": usageFields(summary.Usage),
			"outerUsage": usageFields(state.outerUsage), "visionUsage": usageFields(state.visionUsage),
		}
		if runErr != nil {
			end["status"] = "error"
			end["error"] = runErr.Error()
		}
		if err := errors.Join(log.Event("run-end", end), log.Close()); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("could not complete the audit log: %w", err))
		}
	}()

	if err := log.Event("run-start", map[string]any{
		"flags": map[string]any{
			"prompt": cfg.PromptPath, "allowPath": cfg.AllowPath, "deny": []string{},
			"model": cfg.Model, "visionModel": cfg.VisionModel,
		},
		"cwd": cfg.CWD, "dateTime": cfg.Now.UTC().Format("2006-01-02T15:04:05.000Z"),
		"localDateTime": localDateTime(cfg.Now), "timeZone": cfg.Now.Location().String(),
	}); err != nil {
		return summary, err
	}
	// Notes always reserves the default directory; do not accumulate one
	// exclusion per generated log in a long-running watch process.
	if cfg.LogPath != "" {
		if err := cfg.Notes.SetAuditPath(log.Path); err != nil {
			return summary, fmt.Errorf("could not exclude the audit log from the notes store: %w", err)
		}
	}
	cfg.Notes.SetTrigger(cfg.Trigger)
	if err := state.loop(ctx, &summary); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return summary, fmt.Errorf("run timed out after %dms: %w", cfg.Timeout.Milliseconds(), err)
			}
			return summary, fmt.Errorf("run interrupted: %w", err)
		}
		return summary, err
	}
	return summary, nil
}

type runState struct {
	cfg         Config
	log         *audit.Log
	written     map[string]struct{}
	outerUsage  fireworks.Usage
	visionUsage fireworks.Usage
}

func (s *runState) loop(ctx context.Context, summary *Summary) error {
	req := fireworks.Request{
		Model: s.cfg.Model, MaxTokens: 8192, Tools: toolDefinitions(),
	}
	size := 0
	appendMessage := func(msg fireworks.Message) error {
		// Charge each message and call a fixed overhead as well as its data.
		n := 64 + len(msg.Role) + len(msg.Content) + len(msg.Reasoning) + len(msg.ToolCallID)
		for _, call := range msg.ToolCalls {
			n += 64 + len(call.ID) + len(call.Name) + len(call.Arguments)
		}
		if n > MaxHistory-size {
			return ErrHistoryLimit
		}
		size += n
		req.Messages = append(req.Messages, msg)
		return nil
	}
	if err := appendMessage(fireworks.Message{Role: "system", Content: systemPrompt(s.cfg)}); err != nil {
		return err
	}
	if err := appendMessage(fireworks.Message{Role: "user", Content: userPrompt(s.cfg)}); err != nil {
		return err
	}
	for step := 1; step <= s.cfg.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := s.cfg.ModelClient.Complete(ctx, req)
		s.outerUsage = addUsage(s.outerUsage, resp.Usage)
		if err != nil {
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
				if errors.Is(ctx.Err(), cause) || errors.Is(err, cause) {
					return cause
				}
			}
			// Provider errors may contain request bodies or credentials.
			if _, safe := s.cfg.ModelClient.(*fireworks.Client); safe {
				return fmt.Errorf("%w: %w", ErrModel, err)
			}
			return ErrModel
		}
		summary.Steps = step
		msg := resp.Message
		msg.Role, msg.ToolCallID, msg.ImageURL = "assistant", "", ""
		stepErr := s.processStep(ctx, step, resp.FinishReason, msg, appendMessage)
		if err := s.log.Event("step-finish", map[string]any{"step": step, "usage": usageFields(resp.Usage)}); err != nil {
			stepErr = errors.Join(stepErr, err)
		}
		if stepErr != nil {
			return stepErr
		}
		if len(msg.ToolCalls) == 0 {
			summary.Text = msg.Content
			return nil
		}
	}
	return fmt.Errorf("%w (%d steps) before the agent finished", ErrStepLimit, s.cfg.MaxSteps)
}

func (s *runState) processStep(ctx context.Context, step int, finish string, msg fireworks.Message, appendMessage func(fireworks.Message) error) error {
	if err := appendMessage(msg); err != nil {
		return err
	}
	for _, part := range []struct{ kind, text string }{{"text", msg.Content}, {"reasoning", msg.Reasoning}} {
		if part.text != "" {
			if err := s.log.Event("assistant-text", map[string]any{"kind": part.kind, "step": step, "text": part.text}); err != nil {
				return err
			}
		}
	}
	if finish == "error" {
		return errors.New("the model stopped because of an error")
	}
	toolFinish := finish == "tool_calls" || finish == "tool-calls" || finish == "function_call"
	if toolFinish && len(msg.ToolCalls) == 0 {
		return errors.New("model signaled tool calls but returned none")
	}
	if !toolFinish && len(msg.ToolCalls) != 0 {
		return errors.New("model returned tool calls without a complete tool-call finish; no tools were executed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ids := make(map[string]struct{}, len(msg.ToolCalls))
	for _, call := range msg.ToolCalls {
		_, seen := ids[call.ID]
		if seen || call.ID == "" {
			return errors.New("model returned an empty or duplicate tool call ID")
		}
		ids[call.ID] = struct{}{}
	}
	for _, call := range msg.ToolCalls {
		if err := ctx.Err(); err != nil {
			return err
		}
		toolMessage, err := s.runTool(ctx, step, call)
		if err != nil {
			return err
		}
		if err := appendMessage(toolMessage); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *runState) recordWrite(path string) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.cfg.CWD, path)
	}
	if rel, err := filepath.Rel(s.cfg.CWD, path); err == nil {
		s.written[rel] = struct{}{}
	}
	if s.cfg.OnWrite != nil {
		s.cfg.OnWrite(path)
	}
}

func usageFields(u fireworks.Usage) map[string]int {
	return map[string]int{"inputTokens": u.InputTokens, "outputTokens": u.OutputTokens, "totalTokens": u.TotalTokens}
}

func addUsage(a, b fireworks.Usage) fireworks.Usage {
	return fireworks.Usage{
		InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens: totalTokens(a) + totalTokens(b),
	}
}

func totalTokens(u fireworks.Usage) int {
	if u.TotalTokens == 0 {
		return u.InputTokens + u.OutputTokens
	}
	return u.TotalTokens
}

// FormatSummary renders the one-line stderr summary used by the CLI.
func FormatSummary(s Summary) string {
	plural := func(n int) string {
		if n == 1 {
			return ""
		}
		return "s"
	}
	return fmt.Sprintf("bobik: %d step%s, %d file%s written, %d tokens (in %d, out %d), log %s",
		s.Steps, plural(s.Steps), s.FilesWritten, plural(s.FilesWritten), totalTokens(s.Usage),
		s.Usage.InputTokens, s.Usage.OutputTokens, s.LogRelativePath)
}
