package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tichopad/bobik/internal/notes"
	"golang.org/x/sys/unix"
)

func testSource(t *testing.T, root string, rules ...notes.Rule) *source {
	t.Helper()
	store, err := notes.NewVault(root, rules)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	src, err := newSource(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	return src
}

func TestReconcileRecoversMissedEventsAndDirectoryWatches(t *testing.T) {
	root := t.TempDir()
	changed := filepath.Join(root, "changed.md")
	removed := filepath.Join(root, "removed", "old.md")
	unchanged := filepath.Join(root, "same.md")
	mkdir(t, filepath.Dir(removed))
	write(t, changed, "#bobik original")
	write(t, removed, "#bobik delete")
	write(t, unchanged, "#bobik no change")
	src := testSource(t, root)

	// Simulate a lost event stream by not delivering any native events to the
	// source. Recovery must compare the tree and repair its directory watches.
	write(t, changed, "#bobik changed")
	remove(t, removed)
	remove(t, filepath.Dir(removed))
	created := filepath.Join(root, "added", "nested", "fresh.md")
	mkdir(t, filepath.Dir(created))
	write(t, created, "#bobik new")
	paths := make(map[string]bool)
	if err := src.reconcile(context.Background(), func(path string) { paths[path] = true }); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 || !paths[changed] || !paths[removed] || !paths[created] {
		t.Fatalf("reconciliation queued %v", paths)
	}
	if !slices.Contains(src.WatchList(), filepath.Dir(created)) {
		t.Fatalf("new directory is not watched; watches = %v", src.WatchList())
	}
	if slices.Contains(src.WatchList(), filepath.Dir(removed)) {
		t.Fatalf("removed directory is still watched; watches = %v", src.WatchList())
	}
}

func TestSourceSkipsSymlinkHiddenAuditAndHardlinkEntries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "outside.md"), "#bobik outside")
	for _, name := range []string{".agent-bobik", ".hidden", "normal"} {
		mkdir(t, filepath.Join(root, name))
		write(t, filepath.Join(root, name, "fake.md"), "#bobik note")
	}
	if err := os.Symlink(outside, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outside, "outside.md"), filepath.Join(root, "hard.md")); err != nil {
		t.Fatal(err)
	}
	src := testSource(t, root, notes.Rule{Path: ".hidden", Mode: notes.Hide})
	want := filepath.Join(root, "normal", "fake.md")
	if len(src.known) != 1 || src.known[want] == nil || len(src.WatchList()) != 2 {
		t.Fatalf("unexpected metadata or watches: %v, %v", src.known, src.WatchList())
	}
}

func TestSourceSkipsBindMountWhenPermitted(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkdir(t, filepath.Join(outside, "nested"))
	write(t, filepath.Join(outside, "nested", "outside.md"), "#bobik outside")
	mount := filepath.Join(root, "bind")
	mkdir(t, mount)
	if err := unix.Mount(outside, mount, "", unix.MS_BIND, ""); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("bind mount unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(mount, unix.MNT_DETACH); err != nil {
			t.Error(err)
		}
	})
	src := testSource(t, root)
	if len(src.known) != 0 || len(src.WatchList()) != 1 {
		t.Fatalf("crossed bind mount: notes=%v watches=%v", src.known, src.WatchList())
	}
}

func TestSourceOpenCannotCrossExistingProcMount(t *testing.T) {
	root, err := os.Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var rootStat, procStat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, "/", 0, unix.STATX_MNT_ID, &rootStat); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statx(unix.AT_FDCWD, "/proc", 0, unix.STATX_MNT_ID, &procStat); err != nil {
		t.Skipf("cannot verify existing proc mount: %v", err)
	}
	if rootStat.Mask&unix.STATX_MNT_ID == 0 || procStat.Mask&unix.STATX_MNT_ID == 0 || rootStat.Mnt_id == procStat.Mnt_id {
		t.Skip("no distinct proc mount")
	}
	src := &source{root: "/", rootFile: root}
	if f, err := src.open("/proc", unix.O_RDONLY|unix.O_DIRECTORY); !errors.Is(err, unix.EXDEV) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("cross-mount open returned %v, want EXDEV", err)
	}
}

func TestReadPromptRejectsOversizeAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	large := filepath.Join(root, "large")
	write(t, large, "")
	if err := os.Truncate(large, maxPromptBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrompt(large); err == nil {
		t.Fatal("read an oversized prompt")
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readPrompt(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read a FIFO as prompt")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked opening a FIFO prompt")
	}
}
