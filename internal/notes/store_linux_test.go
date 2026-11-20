//go:build linux

package notes

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func put(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadPathResolutionAndLimit(t *testing.T) {
	root := t.TempDir()
	file := put(t, root, "daily notes/day.md", "daily")
	s := newVault(t, root)
	for _, path := range []string{"daily notes/day.md", file, "./daily notes/sub/../day.md"} {
		if got, err := s.Read(path, 5); err != nil || string(got) != "daily" {
			t.Errorf("Read(%q) = %q, %v", path, got, err)
		}
	}
	for _, path := range []string{"daily notes", "/other/outside.md", "../outside.md", "daily notes/day.md\x00"} {
		if _, err := s.Read(path, -1); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Read(%q) = %v, want ErrNotAllowed", path, err)
		}
	}
	if _, err := s.Read(file, 4); !errors.Is(err, ErrTooLarge) {
		t.Errorf("small Read = %v, want ErrTooLarge", err)
	}
}

func TestSymlinksHardLinksAndSpecialFilesAreNotEligible(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	outside := put(t, external, "secrets.md", "do not touch")
	inside := put(t, root, "real/normal.md", "visible")
	for alias, target := range map[string]string{"external": external, "inside": filepath.Dir(inside), "symlink.md": inside} {
		if err := os.Symlink(target, filepath.Join(root, alias)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(outside, filepath.Join(root, "hardlink.md")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newVault(t, root)
	for _, path := range []string{"external/secrets.md", "inside/normal.md", "symlink.md", "hardlink.md", "fifo.md"} {
		if _, err := store.Read(path, -1); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Read(%q) should reject file: %v", path, err)
		}
		if err := store.Write(path, []byte("modified")); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Write(%q) should reject file: %v", path, err)
		}
		if err := store.Delete(path); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Delete(%q) should reject file: %v", path, err)
		}
	}
	if got, err := store.Read("real/normal.md", -1); err != nil || string(got) != "visible" {
		t.Fatalf("Read visible file = %q, %v", got, err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "do not touch" {
		t.Fatalf("outside file changed: %q, %v", got, err)
	}
	if linkRoot, err := NewVault(filepath.Join(root, "external"), nil); !errors.Is(err, ErrNotAllowed) {
		if linkRoot != nil {
			_ = linkRoot.Close()
		}
		t.Fatalf("symlink root: got %v, want ErrNotAllowed", err)
	}
}

func TestMutationsAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	store := newVault(t, root)
	path := "notes/new/deep/note.md"
	if err := store.Write(path, []byte("dupe\ndupe\nsingle")); err != nil {
		t.Fatal(err)
	}
	if err := store.Edit(path, "dupe", "once"); err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Errorf("duplicate edit: %v", err)
	}
	if err := store.Edit(path, "single", "$&\n"); err != nil {
		t.Fatal(err)
	}
	if count, err := store.EditWithOptions(path, "dupe", "done", true); count != 2 || err != nil {
		t.Fatalf("EditWithOptions = %d, %v", count, err)
	}
	if b, err := store.Read(path, -1); err != nil || string(b) != "done\ndone\n$&\n" {
		t.Errorf("edited content = %q, %v", b, err)
	}
	for _, op := range []func() error{
		func() error { _, err := store.Read("notes/note.md", -1); return err },
		func() error { return store.Edit("notes/note.md", "one", "two") },
		func() error { return store.Delete("notes/note.md") },
	} {
		if err := op(); !errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), path) {
			t.Errorf("missing-path error: %v", err)
		}
	}
	put(t, root, path, "%% #bobik: draft %%")
	for _, op := range []func() error{
		func() error { return store.Write(path, []byte("replace")) },
		func() error { return store.Edit(path, "draft", "change") },
		func() error { return store.Delete(path) },
	} {
		if err := op(); !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), "draft") {
			t.Errorf("draft mutation: %v", err)
		}
	}
	store.SetTrigger("@custom")
	if err := store.Write(path, []byte("%% @custom: draft %%")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(path); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("custom draft not protected: %v", err)
	}
	put(t, root, path, "`%% @custom: sample %%`")
	if err := store.Delete(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("deleted path remains: %v", err)
	}
}
