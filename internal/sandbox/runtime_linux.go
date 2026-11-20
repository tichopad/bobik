package sandbox

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

var tools = []string{
	"bash", "rg", "grep", "cat", "head", "tail", "sort", "uniq", "wc", "cut", "tr", "sed",
}

const libraryPath = "/runtime/lib"

type runtimeProfile struct {
	prlimit string
	bwrap   string
	mounts  []string
}

func loadRuntime(ctx context.Context) (runtimeProfile, error) {
	var profile runtimeProfile
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		return profile, fmt.Errorf("unsupported runtime architecture: %s", runtime.GOARCH)
	}
	binDirs := []string{"/usr/bin", "/bin"}
	var err error
	profile.prlimit, err = findPackageFile(binDirs, "prlimit")
	if err != nil {
		return profile, err
	}
	profile.bwrap, err = findPackageFile(binDirs, "bwrap")
	if err != nil {
		return profile, err
	}

	archDir := map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}[runtime.GOARCH]
	libDirs := []string{
		"/lib/" + archDir, "/usr/lib/" + archDir,
		"/lib64", "/usr/lib64", "/lib", "/usr/lib",
	}
	mounted := make(map[string]string)
	var addELF func(string, string) error
	addELF = func(host, dest string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if existing, ok := mounted[dest]; ok {
			if existing != host {
				return fmt.Errorf("conflicting runtime mount: %s", dest)
			}
			return nil
		}
		mounted[dest] = host
		profile.mounts = append(profile.mounts, "--ro-bind", host, dest)

		interp, needed, err := elfRuntime(host)
		if err != nil {
			return fmt.Errorf("inspect runtime file %s: %w", host, err)
		}
		if interp != "" {
			loader, err := packageFile(interp, libDirs)
			if err != nil {
				return err
			}
			if err := addELF(loader, interp); err != nil {
				return err
			}
		}
		for _, name := range needed {
			if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." {
				return fmt.Errorf("invalid runtime library name: %q", name)
			}
			hostLib, err := findPackageFile(libDirs, name)
			if err != nil {
				return err
			}
			if err := addELF(hostLib, libraryPath+"/"+name); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range tools {
		host, err := findPackageFile(binDirs, name)
		if err != nil {
			return profile, err
		}
		if err := addELF(host, "/bin/"+name); err != nil {
			return profile, err
		}
	}
	return profile, nil
}

func elfRuntime(name string) (string, []string, error) {
	file, err := elf.Open(name)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	// Resolve dependencies from packaged system directories, not from
	// executable-supplied RPATHs, a host linker cache, or by running ldd.
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
		paths, err := file.DynString(tag)
		if err != nil {
			return "", nil, err
		}
		if len(paths) != 0 {
			return "", nil, fmt.Errorf("custom library search paths are not supported")
		}
	}
	var interpreter string
	for _, program := range file.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(program.Open(), 4096))
		if err != nil {
			return "", nil, err
		}
		name, terminated := strings.CutSuffix(string(data), "\x00")
		if !terminated || strings.ContainsRune(name, 0) || !filepath.IsAbs(name) || filepath.Clean(name) != name {
			return "", nil, fmt.Errorf("invalid ELF interpreter")
		}
		interpreter = name
	}
	needed, err := file.ImportedLibraries()
	return interpreter, needed, err
}

func findPackageFile(dirs []string, name string) (string, error) {
	for _, dir := range dirs {
		file, err := packageFile(filepath.Join(dir, name), dirs)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("required distro package file %s: %w", name, fs.ErrNotExist)
}

func packageFile(name string, dirs []string) (string, error) {
	if !inDirectory(name, dirs) {
		return "", fmt.Errorf("unexpected runtime path: %s", name)
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	if !inDirectory(resolved, dirs) {
		return "", fmt.Errorf("runtime path resolves outside package directories: %s", name)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || !info.Mode().IsRegular() ||
		info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|0o022) != 0 {
		return "", fmt.Errorf("runtime file must be an ordinary root-owned package file: %s", name)
	}
	return resolved, nil
}

func inDirectory(name string, dirs []string) bool {
	for _, dir := range dirs {
		if filepath.Dir(name) == dir {
			return true
		}
	}
	return false
}
