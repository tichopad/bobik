//go:build linux

package notes

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

func openStore(t *testing.T, pattern, root string) *Store {
	t.Helper()
	store, err := Open(pattern, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestReadWalkPolicy(t *testing.T) {
	root := t.TempDir()
	daily := put(t, root, "daily notes/day.md", "daily")
	put(t, root, "daily notes/sub/day.md", "nested")
	put(t, root, "daily notes/sub/another.markdown", "another")
	put(t, root, "daily notes/sub/unapproved.txt", "private")
	put(t, root, "daily notes/.draft.md", "hidden")
	put(t, root, "daily notes/.private/day.md", "hidden")
	put(t, root, "daily notes/faux.md/info.md", "beneath a directory")
	put(t, root, "daily notes/excluded/config.md", "config")
	put(t, root, "daily notes/audit.md", "audit")
	put(t, root, "other/outside.md", "private")
	store := openStore(t, filepath.Join(root, `daily\ notes/**/*.{md,markdown}`), root)
	base := filepath.Join(root, "daily notes")
	if got := store.Base(); got != base {
		t.Fatalf("Base = %q, want %q", got, base)
	}
	if err := store.Exclude("daily notes/excluded"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAuditPath(filepath.Join(base, "audit.md")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"daily notes/audit.md", "daily notes/excluded/subfolder", ".agent-bobik/session.jsonl"} {
		if err := store.Exclude(path); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.excluded) != 2 {
		t.Fatalf("stored redundant exclusions: %v", store.excluded)
	}
	if matches, descends := store.WatchPath("day.md"); !matches || !descends {
		t.Errorf("WatchPath(Base-relative day.md) = %v, %v", matches, descends)
	}
	if matches, descends := store.WatchPath(filepath.Join(base, "excluded")); matches || descends {
		t.Errorf("WatchPath(excluded) = %v, %v", matches, descends)
	}
	if matches, descends := store.WatchPath("../other/outside.md"); matches || descends {
		t.Errorf("WatchPath(outside base) = %v, %v", matches, descends)
	}
	for _, path := range []string{"daily notes/day.md", daily, "./daily notes/sub/../day.md"} {
		if got, err := store.Read(path, 5); err != nil || string(got) != "daily" {
			t.Errorf("Read(%q) = %q, %v", path, got, err)
		}
	}
	for _, path := range []string{"day.md", "daily notes/.draft.md", "other/outside.md", "daily notes/audit.md",
		"daily notes/excluded/config.md", "daily notes/faux.md", "daily notes/.private/day.md",
		"daily notes/sub/unapproved.txt", "/other/outside.md", "../outside.md", "daily notes/day.md\x00"} {
		if _, err := store.Read(path, -1); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Read(%q) = %v, want ErrNotAllowed", path, err)
		}
	}
	if _, err := store.Read(daily, 4); !errors.Is(err, ErrTooLarge) {
		t.Errorf("small Read = %v, want ErrTooLarge", err)
	}

	var got []string
	err := store.Walk(t.Context(), func(relative string, info fs.FileInfo) error {
		if !info.Mode().IsRegular() {
			t.Errorf("walk yielded non-regular %q", relative)
		}
		if _, err := store.Read(filepath.Join(base, relative), 256); err != nil {
			t.Errorf("walked file cannot be read: %v", err)
		}
		got = append(got, relative)
		return nil
	})
	slices.Sort(got)
	want := []string{"day.md", "faux.md/info.md", "sub/another.markdown", "sub/day.md"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Walk = %v, %v; want %v", got, err, want)
	}
}

func TestGlobForms(t *testing.T) {
	root := t.TempDir()
	put(t, root, "note.md", "root")
	put(t, root, ".notes/note.md", "hidden dir")
	put(t, root, ".notes/.seed.md", "hidden file")
	put(t, root, ".agent-bobik/log.md", "audit")
	put(t, root, "daily*notes/2026.md", "escaped")
	for _, pattern := range []string{"", "!**/*.md", "foo/../../*.md", filepath.Join(filepath.Dir(root), "*.md"),
		"**/+(test).md", "{one/two,three}/note.md", "{1..3}.md", "foo/[broken", "foo/\x00", `\../*.md`} {
		if s, err := Open(pattern, root); err == nil {
			_ = s.Close()
			t.Errorf("accepted invalid pattern %q", pattern)
		}
	}
	for _, test := range []struct{ pattern, read string }{
		{"**/*.md", "note.md"}, {".notes/**/*.md", ".notes/note.md"},
		{".notes/**/.seed.md", ".notes/.seed.md"}, {`daily\*notes/2026.md`, "daily*notes/2026.md"},
		{"**/{.seed,note}.md", "note.md"}, {".notes/{.seed,note}.md", ".notes/.seed.md"},
	} {
		if _, err := openStore(t, test.pattern, root).Read(test.read, -1); err != nil {
			t.Errorf("%q should allow %q: %v", test.pattern, test.read, err)
		}
	}
	store := openStore(t, ".agent-bobik/**/*.md", root)
	if _, err := store.Read(".agent-bobik/log.md", -1); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("audit file not protected: %v", err)
	}
	if err := store.Walk(t.Context(), func(path string, _ fs.FileInfo) error {
		t.Errorf("walk exposed audit file %q", path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	single := openStore(t, ".notes/note.md", root)
	if got := single.Base(); got != filepath.Join(root, ".notes") {
		t.Errorf("literal file's Base = %q", got)
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
	store := openStore(t, "**/*.md", root)
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
	var seen []string
	if err := store.Walk(t.Context(), func(path string, _ fs.FileInfo) error {
		seen = append(seen, path)
		return nil
	}); err != nil || !slices.Equal(seen, []string{"real/normal.md"}) {
		t.Fatalf("Walk = %q, %v", seen, err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "do not touch" {
		t.Fatalf("outside file changed: %q, %v", got, err)
	}
	if linkRoot, err := Open("**/*.md", filepath.Join(root, "external")); !errors.Is(err, ErrNotAllowed) {
		if linkRoot != nil {
			_ = linkRoot.Close()
		}
		t.Fatalf("symlink root: got %v, want ErrNotAllowed", err)
	}
}

func TestMutationsAndHints(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, "notes/**/*.md", root)
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
		if err := op(); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), path) {
			t.Errorf("missing-path hint: %v", err)
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Walk(ctx, func(string, fs.FileInfo) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled walk: %v", err)
	}
}
