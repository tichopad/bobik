//go:build linux

package notes

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func newVault(t *testing.T, root string, rules ...Rule) *Store {
	t.Helper()
	s, err := NewVault(root, rules)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestVaultDirectoryPolicy(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		".dot/file.txt", ".root", "top.bin", "home/visible.MD", "home/secret/not-a-note",
		"home/reference/manual.txt", "home/reference/private/key", "home/pinned/note.md",
		".agent-bobik/log", ".bobik-write-tmp",
	} {
		put(t, root, path, "old")
	}
	s := newVault(t, root,
		Rule{"./home//secret/", Hide},
		Rule{"home/reference", ReadOnly},
		Rule{"home/reference/private", Hide},
		Rule{"home/pinned", Pin},
		Rule{"home/secret", Hide},
	)
	if s.Base() != root {
		t.Fatalf("Base = %q, want %q", s.Base(), root)
	}
	wantRules := []Rule{
		{".agent-bobik", ReadOnly}, {"home", Pin}, {"home/pinned", Pin},
		{"home/reference", ReadOnly}, {"home/reference/private", Hide}, {"home/secret", Hide},
	}
	if got := s.Rules(); !slices.Equal(got, wantRules) {
		t.Fatalf("Rules = %v, want %v", got, wantRules)
	}
	s.Rules()[0].Mode = Pin
	if got := s.Rules(); !slices.Equal(got, wantRules) {
		t.Fatalf("Rules can be changed by caller: %v", got)
	}

	for _, path := range []string{".dot/file.txt", ".root", "home/pinned/note.md", "home/reference/manual.txt", "home/visible.MD", "top.bin"} {
		if data, err := s.Read(path, -1); err != nil || string(data) != "old" {
			t.Errorf("Read(%q) = %q, %v", path, data, err)
		}
		if match, descend := s.WatchPath(filepath.Join(root, path)); match != strings.EqualFold(filepath.Ext(path), ".md") || !descend {
			t.Errorf("WatchPath(%q) = %v, %v", path, match, descend)
		}
	}
	for _, dir := range []string{root, filepath.Join(root, "home/reference"), ".dot"} {
		if match, descend := s.WatchPath(dir); match || !descend {
			t.Errorf("WatchPath(%q) = %v, %v; want false, true", dir, match, descend)
		}
	}
	for _, path := range []string{filepath.Join(filepath.Dir(root), "outside.md"), "../outside.md"} {
		if match, descend := s.WatchPath(path); match || descend {
			t.Errorf("WatchPath(%q) = %v, %v; want false, false", path, match, descend)
		}
	}
	for _, path := range []string{"home/secret/not-a-note", "home/reference/private/key", ".bobik-write-tmp"} {
		if _, err := s.Read(path, -1); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Read(%q) = %v, want ErrNotAllowed", path, err)
		}
		if match, descend := s.WatchPath(filepath.Join(root, path)); match || descend {
			t.Errorf("WatchPath(%q) = %v, %v", path, match, descend)
		}
	}
	for _, path := range []string{".", root, "home/secret/not-a-note", "home/reference/manual.txt", "home/reference/new/sub/file", ".agent-bobik/log", "home/pinned", "home"} {
		for _, mutate := range []func() error{
			func() error { return s.Write(path, []byte("new")) },
			func() error { return s.Edit(path, "old", "new") },
			func() error { return s.Delete(path) },
		} {
			if err := mutate(); !errors.Is(err, ErrNotAllowed) {
				t.Errorf("mutation of %q = %v, want ErrNotAllowed", path, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "home/reference/new")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("denied write created directories: %v", err)
	}
	if _, err := s.Read("home/reference/private-sibling", -1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("hidden rule affected a similarly named path: %v", err)
	}
	if err := s.Write("home/pinned/new/file.txt", []byte("new")); err != nil {
		t.Fatalf("pin also denied writes inside: %v", err)
	}
	if err := s.Delete("home/pinned/new/file.txt"); err != nil {
		t.Fatalf("pin also denied deletes inside: %v", err)
	}
}

func TestVaultRejectsInvalidRules(t *testing.T) {
	root := t.TempDir()
	put(t, root, "known/deep/file", "data")
	if err := os.Symlink(filepath.Join(root, "known"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	cases := [][]Rule{
		{{"", Hide}}, {{".", Hide}}, {{"/known", Hide}}, {{"../known", Hide}},
		{{"known/../known", Hide}}, {{"known/\x00", Hide}}, {{"known", ""}},
		{{"known", Hide}, {"./known/", Pin}}, {{"known", Hide}, {"known/deep", ReadOnly}},
		{{"known/deep", Pin}, {"known", Hide}}, {{".agent-bobik", Pin}}, {{".agent-bobik/sub", Pin}},
		{{".bobik-write-tmp", Hide}}, {{"absent", Hide}}, {{"alias", Hide}},
		{{"alias/deep", Pin}}, {{"known/deep/file", ReadOnly}},
	}
	for _, rules := range cases {
		s, err := NewVault(root, rules)
		if s != nil {
			_ = s.Close()
		}
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("NewVault(%v) = %v, want ErrNotAllowed", rules, err)
		}
	}
	for _, path := range []string{"", "/", filepath.Join(root, "alias"), filepath.Join(root, "known/deep/file")} {
		s, err := NewVault(path, nil)
		if s != nil {
			_ = s.Close()
		}
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("NewVault(%q) = %v, want ErrNotAllowed", path, err)
		}
	}
}

func TestVaultRejectsMountCrossingRule(t *testing.T) {
	var root, shm unix.Stat_t
	if unix.Stat("/dev", &root) != nil || unix.Stat("/dev/shm", &shm) != nil || root.Dev == shm.Dev {
		t.Skip("/dev/shm is not a distinct mount")
	}
	bad, err := NewVault("/dev", []Rule{{"shm", Hide}})
	if bad != nil {
		_ = bad.Close()
	}
	if !errors.Is(err, ErrNotAllowed) || !errors.Is(err, unix.EXDEV) {
		t.Errorf("NewVault across mount = %v, want ErrNotAllowed and EXDEV", err)
	}
}

func TestVaultImplicitAuditRule(t *testing.T) {
	root := t.TempDir()
	s := newVault(t, root)
	if got := s.Rules(); !slices.Equal(got, []Rule{{".agent-bobik", ReadOnly}}) {
		t.Errorf("Rules = %v", got)
	}
	for _, path := range []string{".agent-bobik", ".agent-bobik/log.jsonl"} {
		if err := s.Write(path, nil); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Write(%q) = %v, want ErrNotAllowed", path, err)
		}
	}
	info, err := os.Stat(filepath.Join(root, ".agent-bobik"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Errorf("reserved audit directory has mode %v", info.Mode())
	}
	if err := s.Write("new.md", nil); err != nil {
		t.Errorf("creation after audit initialization: %v", err)
	}
	if err := os.Remove(filepath.Join(root, ".agent-bobik")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("another.md", nil); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("creation alongside absent audit directory = %v, want ErrNotAllowed", err)
	}
	put(t, root, ".agent-bobik/log.jsonl", "log")
	if data, err := s.Read(".agent-bobik/log.jsonl", -1); err != nil || string(data) != "log" {
		t.Errorf("Read newly created audit log = %q, %v", data, err)
	}
	hidden := newVault(t, root, Rule{".agent-bobik", Hide})
	if _, err := hidden.Read(".agent-bobik/log.jsonl", -1); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("Read explicitly hidden audit log = %v, want ErrNotAllowed", err)
	}
	unsafe := t.TempDir()
	if err := os.Symlink(filepath.Join(root, ".agent-bobik"), filepath.Join(unsafe, ".agent-bobik")); err != nil {
		t.Fatal(err)
	}
	bad, err := NewVault(unsafe, nil)
	if bad != nil {
		_ = bad.Close()
	}
	if !errors.Is(err, ErrNotAllowed) {
		t.Errorf("NewVault with symlink audit directory = %v, want ErrNotAllowed", err)
	}
}

func TestPolicyChecksResolvedAliases(t *testing.T) {
	p, err := compileVault(t.TempDir(), []Rule{{"project/secret", Hide}, {"project/reference", ReadOnly}})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]uint64{
		".agent-bobik": 1, ".AGENT-BOBIK": 1,
		"project": 2, "PROJECT": 2,
		"project/secret": 3, "PROJECT/SECRET~1": 3,
		"project/reference": 4, "PROJECT/REFERENCE": 4,
		"PROJECT/REFERENCE/note.md": 5, "PROJECT/other": 6,
	}
	lookup := func(path string) (resolvedFile, error) {
		if ino, ok := ids[path]; ok {
			return resolvedFile{id: fileID{1, ino}, fd: -1}, nil
		}
		return resolvedFile{fd: -1}, fs.ErrNotExist
	}
	for _, test := range []struct {
		path string
		kind accessKind
		deny bool
	}{
		{".AGENT-BOBIK/log.jsonl", readAccess, false},
		{".AGENT-BOBIK/log.jsonl", writeAccess, true},
		{".AGENT-BOBIK/log.jsonl", watchAccess, true},
		{"PROJECT/SECRET~1/note.md", readAccess, true},
		{"PROJECT/SECRET~1/new.md", writeAccess, true},
		{"PROJECT/REFERENCE/note.md", readAccess, false},
		{"PROJECT/REFERENCE/note.md", writeAccess, true},
		{"PROJECT/REFERENCE/new.md", writeAccess, true},
		{"PROJECT", writeAccess, true},
		{"PROJECT/other/new.md", writeAccess, false},
		{"new.md", writeAccess, false},
	} {
		err := p.checkAliases(test.path, test.kind, lookup)
		if test.deny && !errors.Is(err, ErrNotAllowed) || !test.deny && err != nil {
			t.Errorf("checkAliases(%q, %v) = %v, deny = %t", test.path, test.kind, err, test.deny)
		}
	}
}

func TestVaultChecksCaseAliasesOnCaseInsensitiveFilesystem(t *testing.T) {
	root := t.TempDir()
	put(t, root, ".agent-bobik/log.md", "audit")
	put(t, root, "Project/Secret/note.md", "hidden")
	put(t, root, "Project/Reference/note.md", "read-only")
	if _, err := os.Stat(filepath.Join(root, "PROJECT/secret/note.md")); errors.Is(err, fs.ErrNotExist) {
		t.Skip("temporary filesystem is case-sensitive")
	} else if err != nil {
		t.Fatal(err)
	}
	s := newVault(t, root, Rule{"Project/Secret", Hide}, Rule{"Project/Reference", ReadOnly})
	if data, err := s.Read(".AGENT-BOBIK/log.md", -1); err != nil || string(data) != "audit" {
		t.Errorf("Read through audit alias = %q, %v", data, err)
	}
	if _, err := s.Read("PROJECT/secret/note.md", -1); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("Read through hidden alias = %v, want ErrNotAllowed", err)
	}
	for _, path := range []string{".AGENT-BOBIK/log.md", "PROJECT/secret/note.md"} {
		if match, descend := s.WatchPath(path); match || descend {
			t.Errorf("WatchPath(%q) = %v, %v", path, match, descend)
		}
	}
	if err := s.Write("PROJECT/reference/new.md", nil); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("write through read-only alias = %v, want ErrNotAllowed", err)
	}
	aliased, err := NewVault(root, []Rule{{"Project/Secret", Hide}, {"Project/SECRET", ReadOnly}})
	if aliased != nil {
		_ = aliased.Close()
	}
	if !errors.Is(err, ErrNotAllowed) {
		t.Errorf("duplicate rules through aliases = %v, want ErrNotAllowed", err)
	}
}

func TestVaultPreservesPinnedPathAfterExternalRemoval(t *testing.T) {
	root := t.TempDir()
	put(t, root, "parent/pinned/file", "data")
	s := newVault(t, root, Rule{"parent/pinned", Pin})
	if err := os.Rename(filepath.Join(root, "parent"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"parent", "parent/pinned", "parent/sibling/file"} {
		if err := s.Write(path, nil); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Write(%q) = %v, want ErrNotAllowed", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "parent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("mutation recreated a pinned directory: %v", err)
	}
}
