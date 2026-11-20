package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type MountMode uint8

const (
	Hide MountMode = iota + 1
	ReadOnly
	Pin
)

// Mount describes an existing directory relative to the vault root. Pin
// prevents replacing that directory without making its contents read-only.
type Mount struct {
	Path string
	Mode MountMode
}

// Keep descriptors and argument size below the launcher limits.
const maxMounts = 64

type notesProfile struct {
	args  []string
	files []*os.File
	masks []*os.File
	wants map[string]mountWant
}

type mountWant struct {
	source   *os.File
	readOnly bool
	mask     *os.File
}

func (p notesProfile) close() {
	for _, file := range p.files {
		_ = file.Close()
	}
	for _, file := range p.masks {
		_ = file.Close()
	}
}

func prepareVault(ctx context.Context, vault string, rules []Mount) (notesProfile, error) {
	vault, err := filepath.Abs(vault)
	if err != nil {
		return notesProfile{}, err
	}
	for _, dir := range systemDirs {
		if containsPath(dir, vault) || containsPath(vault, dir) {
			return notesProfile{}, fmt.Errorf("vault overlaps system directory %s", dir)
		}
	}
	root, err := unix.Openat2(unix.AT_FDCWD, vault, &unix.OpenHow{
		Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return notesProfile{}, fmt.Errorf("open vault %s: %w", vault, err)
	}
	p := notesProfile{wants: make(map[string]mountWant)}
	file := os.NewFile(uintptr(root), vault)
	p.bind(file, "/notes", false)
	if err := flockVault(ctx, root); err != nil {
		p.close()
		return notesProfile{}, err
	}

	if err := p.addRules(root, rules); err != nil {
		p.close()
		return notesProfile{}, err
	}
	return p, nil
}

func flockVault(ctx context.Context, root int) error {
	for {
		err := unix.Flock(root, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("lock vault for Bash: %w", err)
		}
		if err := pause(ctx); err != nil {
			return err
		}
	}
}

func pause(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *notesProfile) bind(file *os.File, dest string, readOnly bool) {
	op := "--bind-fd"
	if readOnly {
		op = "--ro-bind-fd"
	}
	p.args = append(p.args, op, strconv.Itoa(6+len(p.files)), dest)
	p.files = append(p.files, file)
	p.wants[dest] = mountWant{source: file, readOnly: readOnly}
}

func (p *notesProfile) addRules(root int, rules []Mount) error {
	if len(rules) > maxMounts {
		return fmt.Errorf("%w: at most %d mount rules", ErrLimit, maxMounts)
	}
	modes := make(map[string]MountMode)
	explicit := make(map[string]bool)
	bytes := 0
	for _, rule := range rules {
		name := rule.Path
		if !filepath.IsLocal(name) || name == "." || name != filepath.Clean(name) || strings.ContainsRune(name, 0) {
			return fmt.Errorf("invalid mount path %q", name)
		}
		if explicit[name] || rule.Mode < Hide || rule.Mode > Pin {
			return fmt.Errorf("duplicate or invalid mount rule for %q", name)
		}
		explicit[name] = true
		modes[name] = rule.Mode
		// Pin the ancestors too; otherwise a rename could move the protected
		// subtree into the writable part of the vault.
		for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
			if modes[dir] != 0 {
				break
			}
			modes[dir] = Pin
		}
		if len(modes) > maxMounts {
			return fmt.Errorf("%w: at most %d mounts including protected ancestors", ErrLimit, maxMounts)
		}
	}
	names := make([]string, 0, len(modes))
	for name := range modes {
		names = append(names, name)
		bytes += len(name)
	}
	if bytes > 64<<10 {
		return fmt.Errorf("%w: combined mount paths exceed 64 KiB", ErrLimit)
	}
	slices.Sort(names)
	for _, name := range names {
		dest := "/notes/" + name
		inheritedRO := false
		for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
			switch modes[dir] {
			case Hide:
				return fmt.Errorf("mount %q is under hidden path %q", name, dir)
			case ReadOnly:
				inheritedRO = true
			}
		}
		fd, err := unix.Openat2(root, name, &unix.OpenHow{
			Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
		})
		if err != nil {
			return fmt.Errorf("open mount target %q: %w", name, err)
		}
		if modes[name] != Hide {
			p.bind(os.NewFile(uintptr(fd), name), dest, inheritedRO || modes[name] == ReadOnly)
			continue
		}
		mask := os.NewFile(uintptr(fd), name)
		p.masks = append(p.masks, mask)
		p.args = append(p.args, "--perms", "000", "--size", "4096", "--tmpfs", dest, "--remount-ro", dest)
		p.wants[dest] = mountWant{readOnly: true, mask: mask}
	}
	return nil
}

func inspectVault(ctx context.Context, dir *os.File, dest string, wants map[string]mountWant) error {
	return inspectBranch(ctx, dir, dest, wants, 0)
}

func inspectBranch(ctx context.Context, dir *os.File, dest string, wants map[string]mountWant, depth int) error {
	const maxDepth = 128
	if depth > maxDepth {
		return fmt.Errorf("%w: vault exceeds %d directory levels at %s", ErrLimit, maxDepth, dest)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, err := dir.Readdirnames(256)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, name := range names {
			child := dest + "/" + name
			if wants[child].mask != nil {
				continue
			}
			var st unix.Stat_t
			if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
				continue // Sync can remove entries during the scan.
			} else if err != nil {
				return fmt.Errorf("inspect %s: %w", child, err)
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				if err != nil {
					return fmt.Errorf("open %s: %w", child, err)
				}
				file := os.NewFile(uintptr(fd), child)
				err = inspectBranch(ctx, file, child, wants, depth+1)
				_ = file.Close()
				if err != nil {
					return err
				}
			case unix.S_IFLNK:
				return fmt.Errorf("vault contains symlink %s", child)
			case unix.S_IFREG:
				if st.Nlink != 1 {
					return fmt.Errorf("vault contains multiply linked file %s", child)
				}
			default:
				return fmt.Errorf("vault contains special file %s", child)
			}
		}
	}
}
