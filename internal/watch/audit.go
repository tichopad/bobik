package watch

import (
	"encoding/json"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// The normal run has already closed its log. Appending is best-effort and
// must neither follow a replacement symlink nor create a new log.
func appendGiveUp(root, path string, paths []string) {
	if path == "" {
		return
	}
	path = absolute(root, path)
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_WRONLY | unix.O_APPEND | unix.O_NONBLOCK | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return
	}
	_ = json.NewEncoder(f).Encode(struct {
		Timestamp string   `json:"ts"`
		Type      string   `json:"type"`
		Paths     []string `json:"paths"`
	}{time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "watch-gave-up", paths})
}
