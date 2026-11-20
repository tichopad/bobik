// Package audit writes one append-only JSONL log per agent run.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const Directory = ".agent-bobik"

// Log writes each event before returning; it holds no log queue or transcript.
type Log struct {
	Path         string
	RelativePath string

	mu       sync.Mutex
	file     *os.File
	writeErr error
}

// New exclusively creates a private log. An empty path generates a timestamped
// filename under cwd/.agent-bobik. An explicit relative path is relative to cwd.
func New(cwd, path string, now time.Time) (*Log, error) {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	explicit := path != ""
	if !explicit {
		path = filepath.Join(cwd, Directory, Filename(now))
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	dir, name := filepath.Split(path)
	if name == "" {
		return nil, errors.New("audit log path must name a file")
	}
	openDir := func(fd int, name string, resolve uint64) (int, error) {
		return unix.Openat2(fd, name, &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_NO_SYMLINKS | resolve,
		})
	}
	var dirFD int
	if explicit {
		dirFD, err = openDir(unix.AT_FDCWD, filepath.Dir(path), 0)
	} else {
		// Create the reserved directory relative to an already pinned CWD.
		cwdFD, openErr := openDir(unix.AT_FDCWD, cwd, 0)
		if openErr != nil {
			return nil, &fs.PathError{Op: "open working directory", Path: cwd, Err: openErr}
		}
		defer unix.Close(cwdFD)
		if err := unix.Mkdirat(cwdFD, Directory, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, &fs.PathError{Op: "create audit directory", Path: dir, Err: err}
		}
		dirFD, err = openDir(cwdFD, Directory, unix.RESOLVE_BENEATH|unix.RESOLVE_NO_XDEV)
	}
	if err != nil {
		return nil, &fs.PathError{Op: "open audit directory", Path: dir, Err: err}
	}
	defer unix.Close(dirFD)
	stem := strings.TrimSuffix(name, ".jsonl")
	for i := 0; i < 100; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d.jsonl", stem, i)
		}
		fd, err := unix.Openat(dirFD, candidate, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
		path := filepath.Join(dir, candidate)
		if err == nil {
			rel, relErr := filepath.Rel(cwd, path)
			if relErr != nil {
				rel = path
			}
			return &Log{Path: path, RelativePath: rel, file: os.NewFile(uintptr(fd), path)}, nil
		}
		if explicit || !errors.Is(err, fs.ErrExist) {
			return nil, &fs.PathError{Op: "create audit log", Path: path, Err: err}
		}
	}
	return nil, fmt.Errorf("create audit log in %s: too many filename collisions", dir)
}

func Filename(now time.Time) string { return now.Format("2006-01-02T15-04-05.000") + ".jsonl" }

func (l *Log) Event(kind string, data map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writeErr != nil {
		// Never append after a failed write: a partial line may be present.
		return l.writeErr
	}
	event := make(map[string]any, len(data)+2)
	for key, value := range data {
		event[key] = value
	}
	event["ts"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	event["type"] = kind
	if err := json.NewEncoder(l.file).Encode(event); err != nil {
		l.writeErr = fmt.Errorf("write audit event %s: %w", kind, err)
	}
	return l.writeErr
}

// Close syncs and closes the log, reporting both errors if necessary.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return errors.Join(l.file.Sync(), l.file.Close())
}
