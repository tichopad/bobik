//go:build linux

// Package notes provides a policy-filtered view of regular files in a vault.
// Paths are resolved with Linux openat2, without fallback.
package notes

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tichopad/bobik/internal/audit"
	"golang.org/x/sys/unix"
)

// Store pins the vault root used for tool paths. Its methods are safe for concurrent
// use. Changes by programs outside Store aren't governed by its mutation lock.
type Store struct {
	policy policy

	mu      sync.Mutex // guards the trigger, fd lifetime, and mutations
	root    *os.File
	trigger string
}

// NewVault opens an existing vault, validates its rules, and reserves the audit directory.
func NewVault(root string, rules []Rule) (*Store, error) {
	p, err := compileVault(root, rules)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, p.root, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, pathError("open vault root (requires Linux openat2)", p.root, err)
	}
	f := os.NewFile(uintptr(fd), p.root)
	seen := make(map[fileID]string, len(p.dirs))
	validate := func(dir string, create bool) error {
		probe, err := openAt(fd, dir, unix.O_PATH|unix.O_DIRECTORY, 0)
		if create && errors.Is(err, fs.ErrNotExist) {
			if err := unix.Mkdirat(fd, dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return pathError("reserve audit directory", filepath.Join(p.root, dir), err)
			}
			probe, err = openAt(fd, dir, unix.O_PATH|unix.O_DIRECTORY, 0)
		}
		if err != nil {
			return fmt.Errorf("%w: invalid vault rule directory %q: %w", ErrNotAllowed, dir, err)
		}
		var st unix.Stat_t
		err = unix.Fstat(int(probe.Fd()), &st)
		probe.Close()
		if err != nil {
			return pathError("stat vault rule", dir, err)
		}
		id := identity(st)
		if prior, ok := seen[id]; ok {
			return fmt.Errorf("%w: vault rules %q and %q alias the same directory", ErrNotAllowed, prior, dir)
		}
		seen[id] = dir
		return nil
	}
	for _, rule := range p.rules() {
		if rule.Path != audit.Directory {
			if err := validate(rule.Path, false); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
	}
	if err := validate(audit.Directory, true); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Store{policy: p, root: f, trigger: DefaultTrigger}, nil
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
func (s *Store) Base() string {
	return s.policy.root
}

// Rules returns a sorted copy of the effective directory rules.
func (s *Store) Rules() []Rule {
	return s.policy.rules()
}

func (s *Store) SetTrigger(trigger string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trigger = trigger
}

// WatchPath reports Markdown eligibility and whether the watcher should look
// for descendants. Watcher reads must still go through Read.
func (s *Store) WatchPath(path string) (matches, descends bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return false, false
	}
	rel, err := relativePath(s.policy.root, path)
	if err != nil || s.policy.hidden(rel) {
		return false, false
	}
	if rel != "." && (access{s.policy, int(s.root.Fd())}).checkAliases(rel, false) != nil {
		return false, false
	}
	return strings.EqualFold(filepath.Ext(rel), ".md"), true
}

type access struct {
	policy
	fd int
}

type fileID struct {
	dev uint64
	ino uint64
}

func identity(st unix.Stat_t) fileID {
	return fileID{uint64(st.Dev), st.Ino}
}

type resolvedFile struct {
	id fileID
	fd int
}

func (f resolvedFile) close() {
	if f.fd >= 0 {
		_ = unix.Close(f.fd)
	}
}

func (a access) resolve(path string) (resolvedFile, error) {
	fd, err := unix.Openat2(a.fd, path, &unix.OpenHow{
		Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: resolveNotes,
	})
	if err != nil {
		return resolvedFile{fd: -1}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return resolvedFile{fd: -1}, err
	}
	return resolvedFile{identity(st), fd}, nil
}

func (a access) checkAliases(path string, mutate bool) error {
	return a.policy.checkAliases(path, mutate, a.resolve)
}

// Only walk branches with rules. Each such branch has a rule at its root
// because ancestors of configured paths are automatically pinned.
func (p policy) checkAliases(path string, mutate bool, lookup func(string) (resolvedFile, error)) error {
	current, prefix := ".", ""
	for part := range strings.SplitSeq(path, "/") {
		children := p.children[current]
		if len(children) == 0 {
			return nil
		}
		prefix = filepath.Join(prefix, part)
		next, err := p.matchAlias(prefix, children, mutate, prefix == path, lookup)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		current = next
	}
	return nil
}

func (p policy) matchAlias(prefix string, children []string, mutate, terminal bool, lookup func(string) (resolvedFile, error)) (string, error) {
	entry, pathErr := lookup(prefix)
	defer entry.close()
	missing := errors.Is(pathErr, fs.ErrNotExist)
	if missing && !mutate {
		return "", nil
	}
	if pathErr != nil && !missing {
		return "", pathErr
	}
	next := ""
	for _, rule := range children {
		id, err := entry.id, pathErr
		if rule != prefix {
			other, lookupErr := lookup(rule)
			id, err = other.id, lookupErr
			other.close()
		}
		if errors.Is(err, fs.ErrNotExist) {
			if missing {
				// Without either inode, a new name may alias the rule.
				return "", fmt.Errorf("%w: cannot create beside missing protected directory %q", ErrNotAllowed, rule)
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if missing || entry.id != id {
			continue
		}
		mode := p.dirs[rule]
		if next != "" || mode == Hide || mutate && (mode == ReadOnly || mode == Pin && terminal) {
			return "", ErrNotAllowed
		}
		next = rule
	}
	return next, nil
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
	return access{s.policy, fd}, nil
}

func (a access) allowedPath(path string) (string, error) {
	rel, err := relativePath(a.root, path)
	if err != nil {
		return "", err
	}
	if rel == "." || a.hidden(rel) {
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

// Read returns the bytes of an eligible regular file without truncation.
// A negative limit disables the byte limit; zero permits only an empty file.
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
	if err := a.checkAliases(rel, false); err != nil {
		return nil, pathError("read", path, err)
	}
	data, _, err := readAt(a.fd, rel, limit)
	if err != nil {
		return nil, pathError("read", path, err)
	}
	return data, nil
}

// ReadPrompt reads the operator-selected prompt, even in a hidden directory.
// It is not an agent tool.
func (s *Store) ReadPrompt(path string, limit int64) ([]byte, error) {
	a, err := s.snapshot()
	if err != nil {
		return nil, pathError("read prompt", path, err)
	}
	defer unix.Close(a.fd)
	rel, err := relativePath(a.root, path)
	if err != nil {
		return nil, pathError("read prompt", path, err)
	}
	data, _, err := readAt(a.fd, rel, limit)
	if err != nil {
		return nil, pathError("read prompt", path, err)
	}
	return data, nil
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
	a := access{s.policy, int(s.root.Fd())}
	rel, err := a.allowedPath(path)
	if err != nil {
		return wrap(err)
	}
	if !a.writable(rel) {
		return wrap(ErrNotAllowed)
	}
	if err := a.checkAliases(rel, true); err != nil {
		return wrap(err)
	}
	dir, name := filepath.Split(rel)
	parent, err := a.parent(filepath.Clean(dir), create)
	if err != nil {
		return wrap(err)
	}
	defer parent.Close()
	fd := int(parent.Fd())

	content, original, err := readAt(fd, name, maxMutationBytes)
	exists := err == nil
	if !exists && !(create && errors.Is(err, fs.ErrNotExist)) {
		return wrap(err)
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
		if err := a.checkAliases(rel, true); err != nil {
			return err
		}
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
	if !a.writable(dir) {
		return nil, ErrNotAllowed
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
