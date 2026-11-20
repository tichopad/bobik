package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var systemDirs = []string{"/usr/bin", "/bin", "/usr/lib", "/lib", "/usr/lib64", "/lib64"}

func runtimePath(path string) bool {
	for _, dir := range systemDirs {
		if containsPath(dir, path) {
			return true
		}
	}
	return false
}

func containsPath(parent, path string) bool {
	return parent == path || parent == "/" || strings.HasPrefix(path, parent+"/")
}

type runtimeProfile struct {
	prlimit string
	bwrap   string
	bash    string
	args    []string
	binds   map[string]string
}

func loadRuntime(ctx context.Context) (runtimeProfile, error) {
	profile := runtimeProfile{binds: make(map[string]string)}
	var err error
	for _, executable := range []struct {
		name   string
		target *string
	}{
		{"prlimit", &profile.prlimit}, {"bwrap", &profile.bwrap}, {"bash", &profile.bash},
	} {
		*executable.target, err = systemExecutable(executable.name)
		if err != nil {
			return profile, err
		}
	}

	// Do not import PATH, /usr/local, linker caches, or other host configuration.
	for _, name := range systemDirs {
		if err := ctx.Err(); err != nil {
			return profile, err
		}
		resolved, err := filepath.EvalSymlinks(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return profile, err
		}
		switch resolved {
		case "/usr/bin", "/bin", "/usr/lib", "/lib", "/usr/lib64", "/lib64":
		default:
			return profile, fmt.Errorf("unexpected system directory partner for %s: %s", name, resolved)
		}
		if err := trustedDirectory(resolved); err != nil {
			return profile, err
		}
		if resolved == name {
			profile.args = append(profile.args, "--ro-bind", resolved, name)
			profile.binds[name] = resolved
		} else {
			profile.args = append(profile.args, "--symlink", resolved, name)
		}
	}
	return profile, nil
}

func systemExecutable(name string) (string, error) {
	for _, dir := range []string{"/usr/bin", "/bin"} {
		resolved, err := filepath.EvalSymlinks(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		dir := filepath.Dir(resolved)
		if dir != "/bin" && dir != "/usr/bin" {
			return "", fmt.Errorf("unexpected path for system executable %s: %s", name, resolved)
		}
		if err := trustedDirectory(dir); err != nil {
			return "", err
		}
		info, err := os.Lstat(resolved)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || !trusted(info) || info.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 ||
			info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("not an ordinary root-owned executable: %s", resolved)
		}
		return resolved, nil
	}
	return "", fmt.Errorf("required system executable %s: %w", name, fs.ErrNotExist)
}

func trustedDirectory(dir string) error {
	for ; ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || !trusted(info) {
			return fmt.Errorf("not a trusted system directory: %s", dir)
		}
		if dir == "/" {
			return nil
		}
	}
}

func trusted(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && info.Mode().Perm()&0o022 == 0
}
