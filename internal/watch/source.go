package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/tichopad/bobik/internal/notes"
	"golang.org/x/sys/unix"
)

// source translates native directory notifications into paths for the scheduler
// to classify. Its initial scan supplies only metadata; existing submissions
// are the startup run's responsibility, not new watch events.
type source struct {
	*fsnotify.Watcher
	ctx        context.Context
	store      *notes.Store
	root, base string
	rootFile   *os.File
	dirs       map[string]fs.FileInfo
	known      map[string]fs.FileInfo
}

func newSource(ctx context.Context, root string, store *notes.Store) (*source, error) {
	base := absolute(root, store.Base())
	if !under(base, root) {
		return nil, fmt.Errorf("watch base %s is outside working directory %s", base, root)
	}
	watcher, err := fsnotify.NewBufferedWatcher(256)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{
		Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		watcher.Close()
		return nil, fmt.Errorf("open watch root %s: %w", root, err)
	}
	src := &source{
		Watcher: watcher, ctx: ctx, store: store, root: root, base: base,
		rootFile: os.NewFile(uintptr(fd), root),
		dirs:     make(map[string]fs.FileInfo), known: make(map[string]fs.FileInfo),
	}
	if err := src.scan(func(path string, info fs.FileInfo) { src.known[path] = info }); err != nil {
		src.Close()
		return nil, err
	}
	return src, nil
}

func (s *source) Close() {
	s.Watcher.Close()
	s.rootFile.Close()
}

func (s *source) event(event fsnotify.Event, queue func(string)) error {
	path := event.Name
	topology := event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)
	if !under(path, s.base) {
		// Ancestors are watched so a missing base can appear later, or a base
		// moved away can be replaced.
		if topology && under(s.base, path) {
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				s.detach(path)
			}
			return s.reconcile(s.ctx, queue)
		}
		return nil
	}
	match, canDescend := s.store.WatchPath(path)
	if topology && (canDescend || s.dirs[path] != nil) {
		if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
			s.detach(path)
		}
		if _, err := s.stat(path, unix.O_DIRECTORY); err == nil && canDescend {
			if path == s.base {
				return s.reconcile(s.ctx, queue)
			}
			return s.scanDir(path, func(path string, info fs.FileInfo) {
				if changed(s.known[path], info) {
					queue(path)
				}
			})
		}
		if s.dirs[path] != nil {
			s.detach(path)
		}
		for name := range s.known {
			if under(name, path) {
				queue(name)
			}
		}
	}
	if match {
		if topology || event.Has(fsnotify.Write) {
			queue(path)
		} else if event.Has(fsnotify.Chmod) && s.known[path] != nil {
			// Linux may emit Chmod before Remove while a deleted inode is open.
			if _, err := s.statNote(path); errors.Is(err, fs.ErrNotExist) {
				queue(path)
			}
		}
	}
	return nil
}

func (s *source) reconcile(ctx context.Context, queue func(string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Kernel state and directory identity can have changed despite no event
	// reaching us. Remove stale registrations before reattaching.
	for dir, prev := range s.dirs {
		if now, err := s.stat(dir, unix.O_DIRECTORY); err != nil || !os.SameFile(prev, now) {
			s.detach(dir)
		}
	}
	current := make(map[string]struct{})
	if err := s.scan(func(path string, info fs.FileInfo) {
		current[path] = struct{}{}
		if changed(s.known[path], info) {
			queue(path)
		}
	}); err != nil {
		return err
	}
	for path := range s.known {
		if _, exists := current[path]; !exists {
			queue(path)
		}
	}
	return nil
}

func changed(prev, now fs.FileInfo) bool {
	if prev == nil || !os.SameFile(prev, now) || prev.Size() != now.Size() || !prev.ModTime().Equal(now.ModTime()) {
		return true
	}
	before, beforeOK := prev.Sys().(*syscall.Stat_t)
	after, afterOK := now.Sys().(*syscall.Stat_t)
	return beforeOK && afterOK && before.Ctim != after.Ctim
}

func (s *source) scan(visit func(string, fs.FileInfo)) error {
	rel, err := filepath.Rel(s.root, s.base)
	if err != nil {
		return err
	}
	dir := s.root
	if rel != "." {
		for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
			f, err := s.attach(dir)
			if err != nil || f == nil {
				return err
			}
			f.Close()
			dir = filepath.Join(dir, part)
		}
	}
	return s.scanDir(dir, visit)
}

func (s *source) scanDir(dir string, visit func(string, fs.FileInfo)) error {
	f, err := s.attach(dir)
	if err != nil || f == nil {
		return err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128)
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			if err := s.ctx.Err(); err != nil {
				return err
			}
			path := filepath.Join(dir, entry.Name())
			match, descends := s.store.WatchPath(path)
			switch {
			case entry.IsDir() && descends:
				if err := s.scanDir(path, visit); err != nil {
					return err
				}
			case entry.Type().IsRegular() && match:
				info, err := s.statNote(path)
				if unavailable(err) {
					continue
				}
				if err != nil {
					return err
				}
				visit(path, info)
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func (s *source) attach(dir string) (*os.File, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.open(dir, unix.O_RDONLY|unix.O_DIRECTORY)
	if unavailable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if prev := s.dirs[dir]; prev != nil {
		if os.SameFile(prev, info) {
			return f, nil
		}
		s.detach(dir)
	}
	if err := s.Add(dir); err != nil {
		f.Close()
		if unavailable(err) {
			return nil, nil
		}
		return nil, err
	}
	// fsnotify registers by path. Don't retain a watch if its name has
	// meanwhile stopped resolving to the safely opened directory.
	if current, err := s.stat(dir, unix.O_DIRECTORY); err != nil || !os.SameFile(info, current) {
		f.Close()
		_ = s.Remove(dir)
		if err != nil && !unavailable(err) {
			return nil, err
		}
		return nil, nil
	}
	if named, err := os.Lstat(dir); err != nil || !os.SameFile(info, named) {
		f.Close()
		_ = s.Remove(dir)
		if err != nil && !unavailable(err) {
			return nil, err
		}
		return nil, nil
	}
	s.dirs[dir] = info
	return f, nil
}

func (s *source) detach(dir string) {
	for path := range s.dirs {
		if under(path, dir) {
			_ = s.Remove(path) // fsnotify may have removed it on unlink.
			delete(s.dirs, path)
		}
	}
}

func (s *source) remember(path string) {
	match, _ := s.store.WatchPath(path)
	if !match {
		return
	}
	if info, err := s.statNote(path); err == nil {
		s.known[path] = info
	} else {
		delete(s.known, path)
	}
}

func unavailable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR) ||
		errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) || errors.Is(err, notes.ErrNotAllowed)
}

func (s *source) open(path string, flags uint64) (*os.File, error) {
	if !under(path, s.root) {
		return nil, notes.ErrNotAllowed
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(int(s.rootFile.Fd()), rel, &unix.OpenHow{
		Flags: flags | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil, &fs.PathError{Op: "open for watch", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (s *source) stat(path string, flags uint64) (fs.FileInfo, error) {
	f, err := s.open(path, unix.O_PATH|flags)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

func (s *source) statNote(path string) (fs.FileInfo, error) {
	info, err := s.stat(path, 0)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 {
		return nil, notes.ErrNotAllowed
	}
	return info, nil
}

func (s *source) removeKnown(path string) bool {
	_, ok := s.known[path]
	delete(s.known, path)
	return ok
}

func under(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimRight(dir, string(filepath.Separator))+string(filepath.Separator))
}
