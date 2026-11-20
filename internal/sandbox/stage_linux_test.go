package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

type fixtureSource struct {
	files fstest.MapFS
	reads []string
	read  func(string, int64) ([]byte, error)
}

func (s *fixtureSource) Walk(ctx context.Context, visit func(string, fs.FileInfo) error) error {
	return fs.WalkDir(s.files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return visit(name, info)
	})
}

func (s *fixtureSource) Read(name string, maxBytes int64) ([]byte, error) {
	s.reads = append(s.reads, name)
	if s.read != nil {
		return s.read(name, maxBytes)
	}
	data, err := s.files.ReadFile(name)
	if int64(len(data)) > maxBytes {
		return nil, ErrLimit
	}
	return data, err
}

type sourceStub struct {
	walk func(context.Context, func(string, fs.FileInfo) error) error
}

func (s sourceStub) Walk(ctx context.Context, visit func(string, fs.FileInfo) error) error {
	return s.walk(ctx, visit)
}

func (sourceStub) Read(string, int64) ([]byte, error) {
	return nil, errors.New("unexpected Read call")
}

func TestStageCopiesOnlyRegularFiles(t *testing.T) {
	source := &fixtureSource{files: fstest.MapFS{
		"project/note.md":  {Data: []byte("needle"), Mode: 0o777},
		"project/link.md":  {Mode: fs.ModeSymlink, Data: []byte("target")},
		"project/pipe.md":  {Mode: fs.ModeNamedPipe},
		"project/.tag.md":  {Data: []byte("tag")},
		"project/emptydir": {Mode: fs.ModeDir},
	}}
	dest := t.TempDir()
	if err := stage(t.Context(), source, dest, defaultStageBudget); err != nil {
		t.Fatal(err)
	}
	if want := []string{"project/.tag.md", "project/note.md"}; !reflect.DeepEqual(source.reads, want) {
		t.Fatalf("read paths = %v, want %v", source.reads, want)
	}
	source.files["project/note.md"].Data[0] = 'N'
	note := filepath.Join(dest, "project/note.md")
	if got, err := os.ReadFile(note); err != nil || string(got) != "needle" {
		t.Fatalf("staged note = %q, %v", got, err)
	}
	if info, err := os.Stat(note); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("staged note mode: %v, %v", info, err)
	}
	for _, omitted := range []string{"link.md", "pipe.md", "emptydir"} {
		if _, err := os.Lstat(filepath.Join(dest, "project", omitted)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s should not be staged: %v", omitted, err)
		}
	}
}

func TestStageRejectsInvalidRelativePath(t *testing.T) {
	info, err := fstest.MapFS{"note": {}}.Stat("note")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../note", "/note", "a/../note", ".", "a\x00b"} {
		t.Run(name, func(t *testing.T) {
			source := sourceStub{walk: func(_ context.Context, visit func(string, fs.FileInfo) error) error {
				return visit(name, info)
			}}
			err := stage(t.Context(), source, t.TempDir(), defaultStageBudget)
			if err == nil || err.Error() != "source returned an invalid relative file path" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestStageRejectsBudgetOverflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget stageBudget
	}{
		{"files", stageBudget{files: 1, dirs: 8, bytes: 10}},
		{"directories", stageBudget{files: 8, dirs: 1, bytes: 10}},
		{"bytes", stageBudget{files: 8, dirs: 8, bytes: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &fixtureSource{files: fstest.MapFS{
				"a/one.md":   {Data: []byte("1")},
				"b/two.md":   {Data: []byte("2")},
				"b/three.md": {Data: []byte("3")},
			}}
			if err := stage(t.Context(), source, t.TempDir(), tc.budget); !errors.Is(err, ErrLimit) {
				t.Fatalf("error = %v, want ErrLimit", err)
			}
		})
	}
	t.Run("file grew after Walk", func(t *testing.T) {
		source := &fixtureSource{
			files: fstest.MapFS{"note.md": {}},
			read: func(_ string, maxBytes int64) ([]byte, error) {
				if maxBytes != 3 {
					t.Errorf("maxBytes = %d, want 3", maxBytes)
				}
				return []byte("123"), nil
			},
		}
		budget := stageBudget{files: 1, dirs: 1, bytes: 2}
		if err := stage(t.Context(), source, t.TempDir(), budget); !errors.Is(err, ErrLimit) {
			t.Fatalf("error = %v, want ErrLimit", err)
		}
	})
}

func TestStageChecksCancelAfterRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &fixtureSource{
		files: fstest.MapFS{"note.md": {}},
		read: func(string, int64) ([]byte, error) {
			cancel()
			return []byte("note"), nil
		},
	}
	dir := t.TempDir()
	err := stage(ctx, source, dir, defaultStageBudget)
	if entries, listErr := os.ReadDir(dir); !errors.Is(err, context.Canceled) || listErr != nil || len(entries) != 0 {
		t.Fatalf("canceled stage: err=%v entries=%v listErr=%v", err, entries, listErr)
	}
}
