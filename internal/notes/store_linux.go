//go:build linux

// Package notes provides an allowlisted view of regular notes beneath a
// working directory. Paths are resolved with Linux openat2, without fallback.
package notes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Store pins the CWD used for tool paths. Its methods are safe for concurrent
// use. Changes by programs outside Store aren't governed by its mutation lock.
type Store struct {
	policy policy

	mu       sync.Mutex // guards configuration, fd lifetime, and mutations
	root     *os.File
	excluded []string
	trigger  string
}

func Open(pattern, cwd string) (*Store, error) {
	p, err := compilePolicy(pattern, cwd)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, p.cwd, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, pathError("open working directory (requires Linux openat2)", p.cwd, err)
	}
	return &Store{policy: p, root: os.NewFile(uintptr(fd), p.cwd), trigger: DefaultTrigger}, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	return err
}

// Base is the absolute directory that anchors the filtered walk and watch.
// Relative paths passed to the typed tools are relative to CWD, not Base.
func (s *Store) Base() string {
	return filepath.Join(s.policy.cwd, s.policy.base)
}

func (s *Store) SetTrigger(trigger string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trigger = trigger
}

// WatchPath reports lexical file eligibility and whether the watcher should
// look for descendants. It also works for missing paths (e.g. on deletion).
// It does not validate an inode; watcher reads still have to go through Read.
func (s *Store) WatchPath(path string) (matches, descends bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return false, false
	}
	// Watch paths, like Walk paths, are relative to Base unless absolute.
	if !filepath.IsAbs(path) {
		rel, err := relativePath(s.Base(), path)
		if err != nil {
			return false, false
		}
		path = filepath.Join(s.Base(), rel)
	}
	rel, err := relativePath(s.policy.cwd, path)
	if err != nil || reserved(rel, s.excluded) {
		return false, false
	}
	return s.policy.candidates(rel)
}

// SetAuditPath adds an audit file or directory to the exclusions. The default
// .agent-bobik directory is reserved regardless of this setting.
func (s *Store) SetAuditPath(path string) error { return s.Exclude(path) }

// Exclude hides a CWD-relative or absolute path and any descendants, including
// from walks and hints. Absolute paths outside CWD are already excluded.
func (s *Store) Exclude(path string) error {
	if path == "" {
		return nil
	}
	if filepath.IsAbs(path) && !strings.ContainsRune(path, 0) {
		rel, err := filepath.Rel(s.policy.cwd, path)
		if err == nil && (rel == ".." || strings.HasPrefix(rel, "../")) {
			return nil
		}
	}
	rel, err := relativePath(s.policy.cwd, path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return fs.ErrClosed
	}
	if reserved(rel, s.excluded) {
		return nil
	}
	s.excluded = slices.DeleteFunc(s.excluded, func(existing string) bool {
		return beneath(existing, rel)
	})
	s.excluded = append(s.excluded, rel)
	return nil
}

type access struct {
	policy
	fd       int
	excluded []string
}

func (s *Store) snapshot() (access, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return access{}, fs.ErrClosed
	}
	fd, err := unix.FcntlInt(s.root.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return access{}, err
	}
	return access{s.policy, fd, slices.Clone(s.excluded)}, nil
}

func (a access) allowedPath(path string) (string, error) {
	rel, err := relativePath(a.cwd, path)
	if err != nil {
		return "", err
	}
	matches, _ := a.candidates(rel)
	if !matches || reserved(rel, a.excluded) {
		return "", ErrNotAllowed
	}
	return rel, nil
}

const resolveNotes = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV

func openAt(dir int, path string, flags uint64, mode uint64) (*os.File, error) {
	fd, err := unix.Openat2(dir, path, &unix.OpenHow{
		Flags: flags | unix.O_CLOEXEC, Mode: mode, Resolve: resolveNotes,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func regular(st *unix.Stat_t) bool {
	// Hard links can give an approved name to an unapproved inode.
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1
}

func pathError(op, path string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTDIR) {
		err = fmt.Errorf("%w: %v", ErrNotAllowed, err)
	}
	return &fs.PathError{Op: op, Path: path, Err: err}
}

func readAt(dir int, path string, limit int64) ([]byte, unix.Stat_t, error) {
	var st unix.Stat_t
	// Reject special files without opening them for I/O. Check the actual
	// data descriptor again: a concurrent rename may have replaced the path.
	probe, err := openAt(dir, path, unix.O_PATH, 0)
	if err != nil {
		return nil, st, err
	}
	err = unix.Fstat(int(probe.Fd()), &st)
	probe.Close()
	if err != nil {
		return nil, st, err
	}
	if !regular(&st) {
		return nil, st, ErrNotAllowed
	}
	// O_NONBLOCK prevents hanging if the path was replaced with a FIFO.
	f, err := openAt(dir, path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, st, err
	}
	defer f.Close()
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return nil, st, err
	}
	if !regular(&st) {
		return nil, st, ErrNotAllowed
	}
	if limit < 0 || limit == math.MaxInt64 {
		data, err := io.ReadAll(f)
		return data, st, err
	}
	if st.Size > limit {
		return nil, st, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, st, ErrTooLarge
	}
	return data, st, err
}

// Read returns the bytes of an eligible regular file, without truncation. A
// negative limit disables the byte limit; zero permits only an empty file.
func (s *Store) Read(path string, limit int64) ([]byte, error) {
	a, err := s.snapshot()
	if err != nil {
		return nil, pathError("read", path, err)
	}
	defer unix.Close(a.fd)
	rel, err := a.allowedPath(path)
	if err != nil {
		return nil, pathError("read", path, err)
	}
	data, _, err := readAt(a.fd, rel, limit)
	if err != nil {
		return nil, a.hint(path, pathError("read", path, err))
	}
	return data, nil
}

const maxHints = 5

func (a access) hint(path string, err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	name := filepath.Base(path)
	var matches []string
	count := 0
	walkErr := a.walk(context.Background(), func(relative string, _ fs.FileInfo) error {
		if filepath.Base(relative) == name {
			count++
			candidate := filepath.Join(a.base, relative)
			matches = append(matches, candidate)
			slices.Sort(matches)
			if len(matches) > maxHints {
				matches = matches[:maxHints]
			}
		}
		return nil
	})
	if walkErr != nil || count == 0 {
		return err
	}
	for i, match := range matches {
		matches[i] = fmt.Sprintf("%q", match)
	}
	where := strings.Join(matches, ", ")
	if count > maxHints {
		where += fmt.Sprintf(", and %d more", count-maxHints)
	}
	subject := "files named"
	verb := "exist"
	if count == 1 {
		subject, verb = "a file named", "exists"
	}
	return fmt.Errorf("%w — %s %q %s at %s; use that path instead of creating a new file", err, subject, name, verb, where)
}

// Walk visits eligible regular files under Base, supplying paths relative to
// Base. Ineligible entries are never sent to the callback. A missing Base is
// an empty walk, allowing first creation in a new directory.
func (s *Store) Walk(ctx context.Context, visit func(relative string, info fs.FileInfo) error) error {
	a, err := s.snapshot()
	if err != nil {
		return err
	}
	defer unix.Close(a.fd)
	return a.walk(ctx, visit)
}

func (a access) walk(ctx context.Context, visit func(string, fs.FileInfo) error) error {
	var walkDir func(string) error
	walkDir = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := openAt(a.fd, dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			if skipWalkEntry(err) {
				return nil
			}
			return pathError("walk", dir, err)
		}
		defer f.Close()
		for {
			names, err := f.Readdirnames(128)
			if err != nil && err != io.EOF {
				return pathError("walk", dir, err)
			}
			for _, name := range names {
				if err := ctx.Err(); err != nil {
					return err
				}
				rel := filepath.Join(dir, name)
				if reserved(rel, a.excluded) {
					continue
				}
				matches, descends := a.candidates(rel)
				if !matches && !descends {
					continue
				}
				info, err := a.metadata(rel)
				if err != nil {
					if skipWalkEntry(err) {
						continue
					}
					return pathError("walk", rel, err)
				}
				if info.IsDir() {
					if descends {
						if err := walkDir(rel); err != nil {
							return err
						}
					}
				} else if matches {
					name, err := filepath.Rel(a.base, rel)
					if err != nil {
						return err
					}
					if err := visit(name, info); err != nil {
						return err
					}
				}
			}
			if err == io.EOF {
				return ctx.Err()
			}
		}
	}
	_, descends := a.candidates(a.base)
	if !descends || reserved(a.base, a.excluded) {
		return ctx.Err()
	}
	return walkDir(a.base)
}

func skipWalkEntry(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotAllowed) ||
		errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV)
}

func (a access) metadata(path string) (fs.FileInfo, error) {
	f, err := openAt(a.fd, path, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR && !regular(&st) {
		return nil, ErrNotAllowed
	}
	return f.Stat()
}

func (s *Store) Write(path string, data []byte) error {
	return s.mutate("write", path, true, func(_ []byte) ([]byte, error) { return data, nil })
}

func (s *Store) Delete(path string) error {
	return s.mutate("delete", path, false, nil)
}

func (s *Store) Edit(path, old, replacement string) error {
	_, err := s.EditWithOptions(path, old, replacement, false)
	return err
}

// EditAll replaces every non-overlapping occurrence of old with replacement.
func (s *Store) EditAll(path, old, replacement string) error {
	_, err := s.EditWithOptions(path, old, replacement, true)
	return err
}

// EditWithOptions returns the number of replacements after a successful edit.
func (s *Store) EditWithOptions(path, old, replacement string, all bool) (int, error) {
	if old == "" {
		return 0, errors.New("oldString must not be empty")
	}
	if old == replacement {
		return 0, errors.New("oldString and newString must differ")
	}
	count := 0
	err := s.mutate("edit", path, false, func(content []byte) ([]byte, error) {
		text := string(content)
		n := strings.Count(text, old)
		switch {
		case n == 0:
			return nil, errors.New("oldString not found")
		case n > 1 && !all:
			return nil, fmt.Errorf("oldString occurs %d times; include more surrounding context to make it unique, or set replaceAll to true", n)
		default:
			// Avoid allocating a replacement that will exceed the write limit.
			delta := len(replacement) - len(old)
			if delta > 0 && n > (maxMutationBytes-len(text))/delta {
				return nil, ErrTooLarge
			}
			count = n
			return []byte(strings.ReplaceAll(text, old, replacement)), nil
		}
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// A large or invalid UTF-8 file cannot be classified as draft-free. Bound the
// mutation read independently of the caller-configurable read/view limits.
const maxMutationBytes = 32 << 20

func (s *Store) mutate(op, path string, create bool, update func([]byte) ([]byte, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wrap := func(err error) error { return pathError(op, path, err) }
	if s.root == nil {
		return wrap(fs.ErrClosed)
	}
	a := access{s.policy, int(s.root.Fd()), s.excluded}
	rel, err := a.allowedPath(path)
	if err != nil {
		return wrap(err)
	}
	dir, name := filepath.Split(rel)
	parent, err := a.parent(filepath.Clean(dir), create)
	if err != nil {
		return a.hint(path, wrap(err))
	}
	defer parent.Close()
	fd := int(parent.Fd())

	content, original, err := readAt(fd, name, maxMutationBytes)
	exists := err == nil
	if !exists && !(create && errors.Is(err, fs.ErrNotExist)) {
		return a.hint(path, wrap(err))
	}
	if exists {
		if !utf8.Valid(content) {
			return wrap(fmt.Errorf("%w: note is not valid UTF-8", ErrNotAllowed))
		}
		if Analyze(string(content), s.trigger).Draft {
			return wrap(fmt.Errorf("%w: protected draft", ErrNotAllowed))
		}
	}

	// Check the pinned parent is still reachable through the approved path,
	// and that the analyzed note has not been modified or replaced. These
	// checks narrow, but cannot close, races with non-cooperating writers.
	recheck := func() error {
		parentNow, err := a.parent(filepath.Clean(dir), false)
		if err != nil {
			return err
		}
		defer parentNow.Close()
		var st, now unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if err := unix.Fstat(int(parentNow.Fd()), &now); err != nil {
			return err
		}
		if st.Dev != now.Dev || st.Ino != now.Ino {
			return fmt.Errorf("%w: parent directory changed during mutation", ErrNotAllowed)
		}
		err = unix.Fstatat(fd, name, &now, unix.AT_SYMLINK_NOFOLLOW)
		if !exists && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !exists || !regular(&now) || original.Dev != now.Dev || original.Ino != now.Ino ||
			original.Ctim != now.Ctim || original.Mtim != now.Mtim || original.Size != now.Size {
			return fmt.Errorf("%w: note changed during mutation", ErrNotAllowed)
		}
		return nil
	}

	if update == nil {
		if err := recheck(); err != nil {
			return wrap(err)
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil {
			return wrap(err)
		}
		return nil
	}
	data, err := update(content)
	if err != nil {
		return wrap(err)
	}
	if len(data) > maxMutationBytes {
		return wrap(ErrTooLarge)
	}
	mode := fs.FileMode(0o600)
	if exists {
		mode = fs.FileMode(original.Mode).Perm()
	}
	if err := publish(fd, name, data, mode, exists, recheck); err != nil {
		return wrap(err)
	}
	return nil
}

func (a access) parent(dir string, create bool) (*os.File, error) {
	f, err := openAt(a.fd, dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if !create || !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	ancestor, err := a.parent(filepath.Dir(dir), true)
	if err != nil {
		return nil, err
	}
	defer ancestor.Close()
	if err := unix.Mkdirat(int(ancestor.Fd()), filepath.Base(dir), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return openAt(a.fd, dir, unix.O_PATH|unix.O_DIRECTORY, 0)
}

func publish(dir int, name string, data []byte, mode fs.FileMode, exists bool, recheck func() error) error {
	var suffix [16]byte
	rand.Read(suffix[:])
	tmp := tempPrefix + hex.EncodeToString(suffix[:])
	f, err := openAt(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(dir, tmp, 0)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Chmod(mode)
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	if err := recheck(); err != nil {
		return err
	}
	if !exists {
		return unix.Renameat2(dir, tmp, dir, name, unix.RENAME_NOREPLACE)
	}
	return unix.Renameat(dir, tmp, dir, name)
}
