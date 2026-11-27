// Package watch runs the agent once at startup and on subsequent note submissions.
package watch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tichopad/bobik/internal/notes"
	"golang.org/x/sys/unix"
)

const DefaultQuietPeriod = 60 * time.Second

type Config struct {
	PromptFile, Trigger string
	// QuietPeriod is the time since the most recent relevant edit. Zero disables it.
	QuietPeriod time.Duration
	Notes       *notes.Store
	Stderr      io.Writer
	// Stdout is available to the caller's Run implementation; watch emits only diagnostics.
	Stdout io.Writer
	// ServiceLog receives operational events when running as a service.
	ServiceLog ServiceLog
	// Close Stop to finish any current run but discard queued work.
	// Cancel the context to abort the current run, which must honor cancellation.
	Stop <-chan struct{}
	// ReadPrompt overrides the default bounded read of PromptFile.
	ReadPrompt func() (string, error)
	// Run receives the reloaded prompt. Return written paths even on failure.
	// Use RecordWrite to suppress notifications before the callback returns.
	Run func(context.Context, string) ([]string, error)
}

type ServiceLog func(event, message string, fields map[string]string)

type ChangedFile struct {
	Path    string
	Deleted bool
}

type runKey struct{}

type runInfo struct {
	root    string
	initial bool
	files   []ChangedFile

	mu      sync.Mutex
	written map[string]struct{}
	logPath string
}

// Changes returns run metadata for a callback invoked by Run.
func Changes(ctx context.Context) (initial bool, files []ChangedFile) {
	if info, _ := ctx.Value(runKey{}).(*runInfo); info != nil {
		return info.initial, append([]ChangedFile(nil), info.files...)
	}
	return false, nil
}

// RecordWrite records an absolute or workspace-relative path written by the agent.
func RecordWrite(ctx context.Context, path string) {
	if info, _ := ctx.Value(runKey{}).(*runInfo); info != nil {
		info.recordWrite(path)
	}
}

// RecordLogPath identifies the audit log to append to if this run exhausts a retry.
func RecordLogPath(ctx context.Context, path string) {
	if info, _ := ctx.Value(runKey{}).(*runInfo); info != nil {
		info.mu.Lock()
		defer info.mu.Unlock()
		info.logPath = path
	}
}

func (info *runInfo) recordWrite(path string) {
	if path == "" {
		return
	}
	path = absolute(info.root, path)
	info.mu.Lock()
	defer info.mu.Unlock()
	info.written[path] = struct{}{}
}

func (info *runInfo) wrote(path string) bool {
	info.mu.Lock()
	defer info.mu.Unlock()
	_, ok := info.written[path]
	return ok
}

type timings struct {
	stable, settle, retry time.Duration
}

func Run(ctx context.Context, cfg Config) error {
	return run(ctx, cfg, timings{
		stable: 500 * time.Millisecond,
		settle: time.Second,
		retry:  time.Minute,
	})
}

type result struct {
	err      error
	batch    batch
	info     *runInfo
	duration time.Duration
}

func run(ctx context.Context, cfg Config, timings timings) (runErr error) {
	if cfg.Notes == nil || cfg.Run == nil {
		return errors.New("watch requires a note store and a run callback")
	}
	root := cfg.Notes.Base()
	cfg.PromptFile = absolute(root, cfg.PromptFile)
	if cfg.ReadPrompt == nil {
		cfg.ReadPrompt = func() (string, error) { return readPrompt(cfg.PromptFile) }
	}
	if cfg.Trigger == "" {
		cfg.Trigger = notes.DefaultTrigger
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	report := func(format string, args ...any) {
		fmt.Fprintf(cfg.Stderr, "bobik: "+format+"\n", args...)
	}
	shuttingDown := func() {
		const message = "shutting down; waiting for the current run to finish"
		if cfg.ServiceLog != nil {
			cfg.ServiceLog("watcher_stopping", message, nil)
		} else {
			report(message)
		}
	}
	watcherError := func(message string, err error) {
		if cfg.ServiceLog != nil {
			cfg.ServiceLog("watcher_error", fmt.Sprintf("%s: %v", message, err), nil)
		} else {
			report("%s: %v", message, err)
		}
	}

	src, err := newSource(ctx, cfg.Notes)
	if err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}
	defer src.Close()

	state := newSchedule(cfg.QuietPeriod, timings.retry)
	if cfg.ServiceLog != nil {
		cfg.ServiceLog("watcher_started", "Watching the vault for submissions", map[string]string{"VAULT": root})
		defer func() {
			outcome := "success"
			if errors.Is(runErr, context.Canceled) {
				outcome = "canceled"
			} else if runErr != nil {
				outcome = "failed"
			}
			cfg.ServiceLog("watcher_stopped", "Vault watcher stopped", map[string]string{"OUTCOME": outcome})
		}()
	} else {
		report("watching %s for changes matching %q", root, cfg.Trigger)
	}
	results := make(chan result, 1)
	var active *runInfo
	var stopping bool
	var stopErr error
	stop := cfg.Stop
	done := ctx.Done()
	events, watchErrors := src.Events, src.Errors

	suppressed := make(map[string]time.Time)
	var files fileDebounce
	files.paths = make(map[string]time.Time)
	queue := func(path string) {
		files.add(path, time.Now().Add(timings.stable))
	}
	allowedChange := func(path string, now time.Time) bool {
		return (active == nil || !active.wrote(path)) && !now.Before(suppressed[path])
	}
	recordRemoval := func(path string) {
		if src.removeKnown(path) {
			state.remove(path)
		}
	}
	examine := func(path string, now time.Time) {
		if !allowedChange(path, now) {
			return
		}
		info, err := src.statNote(path)
		if err != nil {
			if unavailable(err) {
				recordRemoval(path)
			}
			return
		}
		src.known[path] = info
		content, err := readNote(cfg.Notes, path)
		if err != nil {
			state.suppress(path)
			return
		}
		if !notes.Analyze(string(content), cfg.Trigger).Ready {
			state.unready(path)
			return
		}
		if state.ready(path, sha256.Sum256(content), now) && cfg.ServiceLog == nil {
			rel, _ := filepath.Rel(root, path)
			report("run triggered by %s", filepath.ToSlash(rel))
		}
	}
	stopWatching := func() {
		stopping = true
		stop, events, watchErrors = nil, nil, nil
		src.Close()
	}
	checkStop := func() {
		if ctx.Err() != nil && done != nil {
			stopErr = ctx.Err()
			done = nil
			stopWatching()
		} else if stop != nil {
			select {
			case <-stop:
				shuttingDown()
				stopWatching()
			default:
			}
		}
	}

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		// Prioritize shutdown before dispatch even if a deadline is also ready.
		checkStop()
		if stopping && active == nil {
			return stopErr
		}

		now := time.Now()
		var wake <-chan time.Time
		if !stopping {
			for path, until := range suppressed {
				if !now.Before(until) {
					delete(suppressed, path)
				}
			}
			files.flush(now, examine)
			if active == nil && due(state.next(), now) {
				// Multiple native notifications for a rename can postpone
				// an old path's timer past the new path's timer. Include
				// confirmed removals before dispatch without reading
				// or classifying a file that's still being written.
				for path := range files.paths {
					if src.known[path] == nil || !allowedChange(path, now) {
						continue
					}
					if _, err := src.statNote(path); unavailable(err) {
						recordRemoval(path)
					}
				}
				work := state.take(now, func(path string) bool {
					content, err := readNote(cfg.Notes, path)
					return err == nil && notes.Analyze(string(content), cfg.Trigger).Ready
				})
				if work.initial || len(work.paths) != 0 {
					prompt, err := cfg.ReadPrompt()
					checkStop()
					if stopping {
						return stopErr
					}
					if err != nil {
						return fmt.Errorf("could not read prompt file %s: %w; stopping watch mode", cfg.PromptFile, err)
					}
					info := &runInfo{
						root: root, initial: work.initial, files: work.changes(root),
						written: make(map[string]struct{}),
					}
					active = info
					go func() {
						started := time.Now()
						written, err := cfg.Run(context.WithValue(ctx, runKey{}, info), prompt)
						for _, path := range written {
							info.recordWrite(path)
						}
						results <- result{err: err, batch: work, info: info, duration: time.Since(started)}
					}()
				}
			}

			deadline := files.next
			if active == nil {
				deadline = earlier(deadline, state.next())
			}
			if !deadline.IsZero() {
				timer.Reset(time.Until(deadline))
				wake = timer.C
			}
		}

		select {
		case <-done:
			// Finalize cancellation at the top of the loop.
		case <-stop:
			shuttingDown()
			stopWatching()
		case <-wake:
		case event, ok := <-events:
			if !ok {
				stopErr = errors.New("file watcher stopped unexpectedly")
				stopWatching()
			} else if event.Name == root && (event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)) {
				stopErr = errors.New("vault root was removed or moved")
				stopWatching()
			} else if err := src.event(event, queue); err != nil {
				watcherError("watcher error", err)
			}
		case err, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
				break
			}
			watcherError("watcher error", err)
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				if err := src.reconcile(ctx, queue); err != nil {
					watcherError("could not reconcile watcher", err)
				}
			}
		case res := <-results:
			active = nil
			checkStop()
			// The run sends its result after closing its audit.
			res.info.mu.Lock()
			logPath := res.info.logPath
			res.info.mu.Unlock()
			if cfg.ServiceLog != nil {
				reportRun(cfg.ServiceLog, res, logPath)
			}
			if !stopping {
				now := time.Now()
				res.info.mu.Lock()
				for path := range res.info.written {
					suppressed[path] = now.Add(timings.settle)
					state.ignoreWrite(path, res.batch)
					files.discard(path)
					src.remember(path)
				}
				res.info.mu.Unlock()
				if res.err != nil {
					if cfg.ServiceLog == nil {
						report("run failed: %v", res.err)
					}
					gaveUp, retry := state.failed(res.batch, now)
					if len(gaveUp) != 0 {
						for i, path := range gaveUp {
							if path != initialLabel {
								rel, _ := filepath.Rel(root, path)
								gaveUp[i] = filepath.ToSlash(rel)
							}
						}
						if cfg.ServiceLog != nil {
							cfg.ServiceLog("retry_exhausted", "Run retry exhausted; edit the note again to resubmit", nil)
						} else {
							report("gave up on %s after one retry; edit the file again to re-trigger", strings.Join(gaveUp, ", "))
						}
						appendGiveUp(root, logPath, gaveUp)
					}
					if retry {
						if cfg.ServiceLog != nil {
							cfg.ServiceLog("retry_scheduled", "Run retry scheduled", map[string]string{
								"RETRY_DELAY_MS": strconv.FormatInt(timings.retry.Milliseconds(), 10),
							})
						} else {
							report("retrying in %.0fs", math.Round(timings.retry.Seconds()))
						}
					}
				}
			}
		}
		timer.Stop()
	}
}

func reportRun(log ServiceLog, res result, auditPath string) {
	trigger := "note_change"
	if res.batch.initial {
		trigger = "startup"
	}
	outcome, message := "success", "Run finished"
	if errors.Is(res.err, context.Canceled) {
		outcome, message = "canceled", "Run canceled"
	} else if res.err != nil {
		outcome, message = "failed", "Run failed"
	}
	fields := map[string]string{
		"TRIGGER":     trigger,
		"OUTCOME":     outcome,
		"DURATION_MS": strconv.FormatInt(res.duration.Milliseconds(), 10),
	}
	if auditPath != "" {
		fields["AUDIT_PATH"] = auditPath
	} else if res.err != nil {
		message += ": " + res.err.Error()
	}
	log("run_finished", message, fields)
}

// Limits the amount buffered for classification, independently of note-tool limits.
const maxWatchedNoteBytes = 16 << 20
const maxPromptBytes = 1 << 20

func readNote(store *notes.Store, path string) ([]byte, error) {
	return store.Read(path, maxWatchedNoteBytes)
}

func readPrompt(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("prompt must be a regular file")
	}
	if info.Size() > maxPromptBytes {
		return "", errors.New("prompt exceeds the 1 MiB size limit")
	}
	content, err := io.ReadAll(io.LimitReader(f, maxPromptBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > maxPromptBytes {
		return "", errors.New("prompt exceeds the 1 MiB size limit")
	}
	return string(content), nil
}

func absolute(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}

func due(at, now time.Time) bool { return !at.IsZero() && !now.Before(at) }

func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

type fileDebounce struct {
	paths map[string]time.Time
	next  time.Time
}

func (f *fileDebounce) add(path string, at time.Time) {
	f.paths[path] = at
	f.next = earlier(f.next, at)
}

func (f *fileDebounce) discard(path string) { delete(f.paths, path) }

func (f *fileDebounce) flush(now time.Time, visit func(string, time.Time)) {
	if !due(f.next, now) {
		return
	}
	f.next = time.Time{}
	for path, at := range f.paths {
		if due(at, now) {
			delete(f.paths, path)
			visit(path, now)
		} else {
			f.next = earlier(f.next, at)
		}
	}
}
