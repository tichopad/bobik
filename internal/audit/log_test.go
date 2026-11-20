package audit

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogStreamsCompleteEventsAndUsesPrivateUniqueFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 19, 19, 55, 0, 123e6, time.FixedZone("CEST", 7200))
	log, err := New(root, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := log.RelativePath, ".agent-bobik/2026-10-19T19-55-00.123.jsonl"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	other, err := New(root, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(other.Path, "19-55-00.123-1.jsonl") {
		t.Fatalf("collision path = %q", other.Path)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	payload := strings.Repeat("notes\n", 15_000)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if err := log.Event("test", map[string]any{"type": "spoof", "ts": "spoof", "payload": payload}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	info, err := os.Stat(log.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("audit mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(log.Path) // before Close
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("wanted five complete lines, got %d", len(lines))
	}
	for _, line := range lines {
		var event map[string]string
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != "test" || event["payload"] != payload || event["ts"] == "spoof" {
			t.Fatal("audit fields do not match")
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLogRejectsDirectorySymlinkAndRefusesToOverwrite(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, Directory)); err != nil {
		t.Fatal(err)
	}
	if log, err := New(root, "", time.Now()); err == nil {
		log.Close()
		t.Fatal("followed audit directory symlink")
	}
	log, err := New(root, "special.jsonl", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root, "special.jsonl", time.Now()); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("duplicate explicit path: %v", err)
	}
	if err := log.Event("late", nil); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}
