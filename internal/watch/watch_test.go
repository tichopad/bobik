package watch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tichopad/bobik/internal/notes"
)

type testRun struct {
	prompt  string
	initial bool
	files   []ChangedFile
}

type harness struct {
	root, prompt string
	runs         chan testRun
	reports      chan string
	graceful     chan struct{}
	abort        context.CancelFunc
	stopped      chan struct{}
	err          error
}

type reportWriter chan string

func (w reportWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func TestServiceReportsRunFinishingDuringShutdown(t *testing.T) {
	type event struct {
		name   string
		fields map[string]string
	}
	events := make(chan event, 8)
	finish := make(chan struct{})
	h := startTestWatch(t, func(cfg *Config) {
		cfg.ServiceLog = func(name, _ string, fields map[string]string) {
			events <- event{name, fields}
		}
	}, func(ctx context.Context, run testRun) ([]string, error) {
		if run.initial {
			return nil, nil
		}
		select {
		case <-finish:
			RecordLogPath(ctx, "/vault/.agent-bobik/audit.jsonl")
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	next := func(name string) map[string]string {
		t.Helper()
		select {
		case got := <-events:
			if got.name != name {
				t.Fatalf("event = %q, want %q", got.name, name)
			}
			return got.fields
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s", name)
			return nil
		}
	}
	next("watcher_started")
	if fields := next("run_started"); fields["TRIGGER"] != "startup" {
		t.Fatalf("startup fields: %v", fields)
	}
	h.nextRun(t)
	next("run_finished")
	write(t, filepath.Join(h.root, "request.md"), "hey #bobik!")
	if fields := next("run_started"); fields["TRIGGER"] != "note_change" {
		t.Fatalf("note-change fields: %v", fields)
	}
	h.nextRun(t)
	close(h.graceful)
	next("watcher_stopping")
	close(finish)
	fields := next("run_finished")
	if fields["OUTCOME"] != "success" || fields["TRIGGER"] != "note_change" ||
		fields["AUDIT_PATH"] != "/vault/.agent-bobik/audit.jsonl" || fields["DURATION_MS"] == "" {
		t.Fatalf("run fields: %v", fields)
	}
	next("watcher_stopped")
	h.waitStop(t)
}

func startTestWatch(t *testing.T, configure func(*Config), callback func(context.Context, testRun) ([]string, error)) *harness {
	t.Helper()
	h := &harness{
		root: t.TempDir(), runs: make(chan testRun, 20), reports: make(chan string, 100),
		graceful: make(chan struct{}), stopped: make(chan struct{}),
	}
	h.prompt = filepath.Join(t.TempDir(), "prompt.md")
	write(t, h.prompt, "original prompt")
	store, err := notes.NewVault(h.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.abort = cancel
	cfg := Config{
		PromptFile: h.prompt, Notes: store,
		Stop: h.graceful, Stdout: io.Discard, Stderr: reportWriter(h.reports),
		Run: func(ctx context.Context, prompt string) ([]string, error) {
			initial, files := Changes(ctx)
			run := testRun{prompt, initial, files}
			h.runs <- run
			if callback != nil {
				return callback(ctx, run)
			}
			return nil, nil
		},
	}
	if configure != nil {
		configure(&cfg)
	}
	go func() {
		h.err = run(ctx, cfg, timings{stable: 15 * time.Millisecond, settle: 50 * time.Millisecond, retry: 100 * time.Millisecond})
		close(h.stopped)
	}()
	t.Cleanup(func() {
		cancel()
		h.waitStop(t)
		store.Close()
	})
	return h
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func rename(t *testing.T, old, new string) {
	t.Helper()
	if err := os.Rename(old, new); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) nextRun(t *testing.T) testRun {
	t.Helper()
	select {
	case run := <-h.runs:
		return run
	case <-h.stopped:
		t.Fatalf("watch stopped unexpectedly: %v", h.err)
	case <-time.After(3 * time.Second):
		t.Fatal("waited for agent run")
	}
	return testRun{}
}

func (h *harness) waitStop(t *testing.T) error {
	t.Helper()
	select {
	case <-h.stopped:
		return h.err
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop")
		return nil
	}
}

func (h *harness) waitReport(t *testing.T, fragment string) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case line := <-h.reports:
			if strings.Contains(line, fragment) {
				return
			}
		case <-timeout:
			t.Fatalf("waited for report %q", fragment)
		}
	}
}

func (h *harness) noRun(t *testing.T) {
	t.Helper()
	select {
	case run := <-h.runs:
		t.Fatalf("unexpected run: %+v", run)
	case <-h.stopped:
		t.Fatalf("watch stopped unexpectedly: %v", h.err)
	case <-time.After(170 * time.Millisecond):
	}
}

func expectFiles(t *testing.T, run testRun, files ...ChangedFile) {
	t.Helper()
	if run.initial || !reflect.DeepEqual(run.files, files) {
		t.Fatalf("got run %+v, want non-initial run with files %+v", run, files)
	}
}

func TestNativeWatchAtomicSaveAndMove(t *testing.T) {
	h := startTestWatch(t, nil, nil)
	if run := h.nextRun(t); !run.initial || run.files != nil || run.prompt != "original prompt" {
		t.Fatalf("unexpected startup run: %+v", run)
	}

	// Move in a populated tree before there are any watches beneath it.
	incoming := filepath.Join(t.TempDir(), "incoming")
	mkdir(t, filepath.Join(incoming, "nested"))
	write(t, filepath.Join(incoming, "nested", "first.md"), "#bobik request\n")
	write(t, filepath.Join(incoming, "nested", "ignored.txt"), "#bobik\n")
	rename(t, incoming, filepath.Join(h.root, "notes"))
	const first = "notes/nested/first.md"
	expectFiles(t, h.nextRun(t), ChangedFile{Path: first})
	h.noRun(t)

	write(t, h.prompt, "edited prompt")
	tmp := filepath.Join(h.root, "temp.tmp")
	write(t, tmp, "#bobik updated")
	rename(t, tmp, filepath.Join(h.root, first))
	run := h.nextRun(t)
	expectFiles(t, run, ChangedFile{Path: first})
	if run.prompt != "edited prompt" {
		t.Fatalf("prompt not reloaded: %q", run.prompt)
	}
	write(t, filepath.Join(h.root, first), "#bobik updated")
	h.noRun(t)

	// Rename within the tree: old descendant paths are deletion context,
	// while the files in the moved directory can trigger by their new names.
	rename(t, filepath.Join(h.root, "notes/nested"), filepath.Join(h.root, "notes/new"))
	expectFiles(t, h.nextRun(t),
		ChangedFile{Path: "notes/new/first.md"}, ChangedFile{Path: first, Deleted: true})

	remove(t, filepath.Join(h.root, "notes/new/first.md"))
	h.noRun(t)
	write(t, filepath.Join(h.root, "notes/new/other.md"), "#bobik new")
	expectFiles(t, h.nextRun(t),
		ChangedFile{Path: "notes/new/other.md"}, ChangedFile{Path: "notes/new/first.md", Deleted: true})
}

func TestNativeWatchDraftRevalidationAndTrigger(t *testing.T) {
	release := make(chan struct{})
	h := startTestWatch(t, func(cfg *Config) { cfg.Trigger = "@custom" }, func(ctx context.Context, run testRun) ([]string, error) {
		if run.initial {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil, nil
	})
	h.nextRun(t)
	mkdir(t, filepath.Join(h.root, "notes"))
	const path = "notes/draft.md"
	file := filepath.Join(h.root, path)
	write(t, file, "prefix@custom-suffix")
	h.waitReport(t, "run triggered by "+path)
	write(t, file, "prefix@custom-suffix\n%% @custom drafting %%")
	close(release)
	h.noRun(t)
	write(t, file, "prefix@custom-suffix")
	expectFiles(t, h.nextRun(t), ChangedFile{Path: path})
}

func TestNativeWatchAgentWritesAndCoalescing(t *testing.T) {
	release := make(chan struct{})
	writeReady := make(chan error, 1)
	root := make(chan string, 1)
	h := startTestWatch(t, nil, func(ctx context.Context, run testRun) ([]string, error) {
		if !run.initial {
			return nil, nil
		}
		select {
		case dir := <-root:
			RecordWrite(ctx, "notes/agent.md")
			writeReady <- errors.Join(
				os.WriteFile(filepath.Join(dir, "notes/agent.md"), []byte("#bobik mine"), 0o600),
				os.WriteFile(filepath.Join(dir, "notes/late.md"), []byte("#bobik late"), 0o600),
			)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		// late.md is intentionally not reported until the callback returns.
		return []string{"notes/late.md"}, nil
	})
	h.nextRun(t)
	mkdir(t, filepath.Join(h.root, "notes"))
	root <- h.root
	if err := <-writeReady; err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.root, "notes/a.md"), "#bobik a")
	write(t, filepath.Join(h.root, "notes/b.md"), "#bobik b")
	h.waitReport(t, "run triggered by notes/late.md")
	// The test leaves time for all notifications while the first run is held.
	h.noRun(t)
	close(release)
	expectFiles(t, h.nextRun(t), ChangedFile{Path: "notes/a.md"}, ChangedFile{Path: "notes/b.md"})
	h.noRun(t)
}

func TestNativeWatchGiveUpAuditAndPromptError(t *testing.T) {
	logPath := make(chan string, 1)
	h := startTestWatch(t, nil, func(ctx context.Context, run testRun) ([]string, error) {
		if run.initial {
			return nil, nil
		}
		select {
		case path := <-logPath:
			RecordLogPath(ctx, path)
			logPath <- path
			return nil, errors.New("run test failure")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	h.nextRun(t)
	log := filepath.Join(h.root, ".agent-bobik", "failed.jsonl")
	mkdir(t, filepath.Dir(log))
	write(t, log, "{}\n")
	logPath <- log
	mkdir(t, filepath.Join(h.root, "notes"))
	write(t, filepath.Join(h.root, "notes/submission.md"), "#bobik submit")
	h.nextRun(t)
	h.waitReport(t, "retrying")
	h.nextRun(t)
	h.waitReport(t, "gave up on notes/submission.md after one retry")
	h.noRun(t)
	data, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(data), `"type":"watch-gave-up","paths":["notes/submission.md"]`) {
		t.Fatalf("missing give-up audit event: %q (%v)", data, err)
	}
	remove(t, h.prompt)
	write(t, filepath.Join(h.root, "notes/another.md"), "#bobik new")
	if err := h.waitStop(t); err == nil || !strings.Contains(err.Error(), "could not read prompt file") {
		t.Fatalf("want a fatal prompt error, got %v", err)
	}
}

func TestNativeWatchTwoStageShutdown(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "graceful"
		if force {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			canceled := make(chan struct{})
			h := startTestWatch(t, nil, func(ctx context.Context, _ testRun) ([]string, error) {
				select {
				case <-ctx.Done():
					close(canceled)
					return nil, ctx.Err()
				case <-release:
					return nil, nil
				}
			})
			h.nextRun(t)
			mkdir(t, filepath.Join(h.root, "notes"))
			write(t, filepath.Join(h.root, "notes/submission.md"), "#bobik queued")
			h.waitReport(t, "run triggered by")
			close(h.graceful)
			h.waitReport(t, "waiting for the current run to finish")
			select {
			case <-canceled:
				t.Fatal("first signal canceled the active run")
			case <-h.stopped:
				t.Fatal("first signal did not wait for the active run")
			default:
			}
			if force {
				h.abort()
				if err := h.waitStop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("forced stop returned %v", err)
				}
				select {
				case <-canceled:
				default:
					t.Fatal("forced stop did not cancel the active run")
				}
			} else {
				close(release)
				if err := h.waitStop(t); err != nil {
					t.Fatalf("graceful stop returned %v", err)
				}
			}
			select {
			case run := <-h.runs:
				t.Fatalf("shutdown dispatched queued run: %+v", run)
			default:
			}
		})
	}
}
