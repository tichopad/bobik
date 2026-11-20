package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type mountEntry struct {
	device, root, point, fsType         string
	readOnly, nodev, nosuid, nonPrivate bool
}

var mountUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func readMounts(path string) ([]mountEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var mounts []mountEntry
	for scanner.Scan() {
		f := strings.Split(scanner.Text(), " ")
		sep := slices.Index(f, "-")
		if sep < 6 || sep+3 >= len(f) {
			return nil, fmt.Errorf("malformed mountinfo entry")
		}
		opts := strings.Split(f[5], ",")
		m := mountEntry{
			device: f[2], root: mountUnescape.Replace(f[3]), point: mountUnescape.Replace(f[4]),
			fsType: f[sep+1], readOnly: slices.Contains(opts, "ro"),
			nodev: slices.Contains(opts, "nodev"), nosuid: slices.Contains(opts, "nosuid"),
		}
		for _, field := range f[6:sep] {
			if strings.HasPrefix(field, "shared:") || strings.HasPrefix(field, "master:") ||
				strings.HasPrefix(field, "propagate_from:") {
				m.nonPrivate = true
			}
		}
		mounts = append(mounts, m)
	}
	return mounts, scanner.Err()
}

func checkHostMounts(profile runtimeProfile, notes notesProfile) error {
	mounts, err := readMounts("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		return errors.New("host mount topology is empty")
	}
	for _, root := range profile.binds {
		if err := rejectSubmounts(mounts, root); err != nil {
			return err
		}
	}
	vault := notes.files[0].Name()
	if err := rejectSubmounts(mounts, vault); err != nil {
		return err
	}
	vaultDev, vaultRoot := mountCoordinate(mounts, vault)
	for _, dir := range profile.binds {
		dev, root := mountCoordinate(mounts, dir)
		if dev == vaultDev && (containsPath(root, vaultRoot) || containsPath(vaultRoot, root)) {
			return fmt.Errorf("vault aliases system directory %s", dir)
		}
		if err := checkPrivateIdentities([]string{dir}, vault); err != nil {
			return err
		}
		if err := checkPrivateIdentities([]string{vault}, dir); err != nil {
			return err
		}
	}
	return nil
}

func rejectSubmounts(mounts []mountEntry, root string) error {
	for _, m := range mounts {
		if m.point != root && containsPath(root, m.point) {
			return fmt.Errorf("host submount %s under %s", m.point, root)
		}
	}
	return nil
}

func mountCoordinate(mounts []mountEntry, path string) (device, root string) {
	var best mountEntry
	for _, mount := range mounts {
		if len(mount.point) >= len(best.point) && containsPath(mount.point, path) {
			best = mount
		}
	}
	return best.device, filepath.Join(best.root, strings.TrimPrefix(path, best.point))
}

// CheckPrivatePath rejects a path exposed by a vault or system runtime bind,
// including bind-mount aliases. Both paths must be absolute and symlink-resolved.
// The private path itself need not exist.
func CheckPrivatePath(vault, path string) error {
	if !filepath.IsAbs(vault) || !filepath.IsAbs(path) {
		return errors.New("private path and vault must be absolute")
	}
	vault, path = filepath.Clean(vault), filepath.Clean(path)
	if containsPath(vault, path) || runtimePath(path) {
		return fmt.Errorf("private file lies inside the Bash filesystem view: %s", path)
	}
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink != 1 {
		return fmt.Errorf("private file is multiply linked: %s", path)
	}
	mounts, err := readMounts("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	roots := []string{vault}
	for _, dir := range systemDirs {
		resolved, err := filepath.EvalSymlinks(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		roots = append(roots, resolved)
	}
	if err := checkPrivateIdentities(roots, path); err != nil {
		return err
	}
	return checkPrivateCoordinates(mounts, roots, path)
}

// Inode identity also catches alternate names on case-insensitive filesystems.
func checkPrivateIdentities(exposed []string, path string) error {
	roots := make(map[string]fs.FileInfo, len(exposed))
	for _, dir := range exposed {
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		roots[dir] = info
	}
	for ancestor := path; ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Stat(ancestor)
		if err == nil {
			for dir, root := range roots {
				if os.SameFile(info, root) {
					return fmt.Errorf("%s is inside the exposed path %s", path, dir)
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if ancestor == "/" {
			return nil
		}
	}
}

func checkPrivateCoordinates(mounts []mountEntry, exposed []string, path string) error {
	dev, name := mountCoordinate(mounts, path)
	if dev == "" {
		return fmt.Errorf("cannot determine filesystem for private file %s", path)
	}
	for _, dir := range exposed {
		mountDev, root := mountCoordinate(mounts, dir)
		if mountDev == "" {
			return fmt.Errorf("cannot determine filesystem for %s", dir)
		}
		if mountDev == dev && containsPath(root, name) {
			return fmt.Errorf("private file %s aliases a path inside %s", path, dir)
		}
	}
	return nil
}

// Bubblewrap's bind mounts are recursive. Verify the child's final, private
// mount namespace before releasing it; a host mountinfo snapshot isn't enough.
func verifySandbox(ctx context.Context, pid int, profile runtimeProfile, notes notesProfile) error {
	proc, err := os.Open("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return err
	}
	defer proc.Close()
	base := "/proc/self/fd/" + strconv.FormatUint(uint64(proc.Fd()), 10)
	var mounts []mountEntry
	for {
		mounts, err = readMounts(base + "/mountinfo")
		if err != nil {
			return fmt.Errorf("read sandbox mount topology: %w", err)
		}
		final, roots := false, 0
		for _, m := range mounts {
			// Before the second pivot, the sealed root is at /newroot.
			if m.point == "/" {
				roots++
				final = final || m.fsType == "tmpfs" && m.readOnly
			}
		}
		// The second pivot briefly stacks the old root at /.
		if final && roots == 1 {
			break
		}
		if err := pause(ctx); err != nil {
			return err
		}
	}
	if err := verifyMountTable(mounts, profile, notes); err != nil {
		return err
	}
	root := base + "/root"
	for {
		file, err := os.Open(root)
		if err == nil {
			_ = file.Close()
			break
		}
		if !errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("open sandbox root: %w", err)
		}
		// The final pivot precedes Bubblewrap making itself dumpable.
		if waitErr := pause(ctx); waitErr != nil {
			return errors.Join(fmt.Errorf("open sandbox root: %w", err), waitErr)
		}
	}
	if err := checkNull(root + "/dev/null"); err != nil {
		return err
	}
	for dest, want := range notes.wants {
		if want.source == nil {
			continue
		}
		src, err := want.source.Stat()
		if err != nil {
			return err
		}
		got, err := os.Stat(root + dest)
		if err != nil || !os.SameFile(src, got) {
			return fmt.Errorf("sandbox mount %s does not match its pinned source: %v", dest, err)
		}
	}
	dir, err := os.Open(root + "/notes")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := inspectVault(ctx, dir, "/notes", notes.wants); err != nil {
		return err
	}
	for dest, want := range notes.wants {
		source := want.source
		if want.mask != nil {
			source = want.mask
		}
		path := notes.files[0].Name()
		if dest != "/notes" {
			path = filepath.Join(path, strings.TrimPrefix(dest, "/notes/"))
		}
		if err := checkSourcePath(source, path); err != nil {
			return err
		}
	}
	// A trusted sync process could have moved or replaced a mount target
	// during the scan. Recheck the names before giving the shell control.
	mounts, err = readMounts(base + "/mountinfo")
	if err != nil {
		return err
	}
	return verifyMountTable(mounts, profile, notes)
}

func checkSourcePath(source *os.File, path string) error {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return fmt.Errorf("recheck vault mount target %s: %w", path, err)
	}
	target := os.NewFile(uintptr(fd), path)
	defer target.Close()
	old, err := source.Stat()
	if err != nil {
		return err
	}
	now, err := target.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(old, now) {
		return fmt.Errorf("vault mount target %s changed during startup", path)
	}
	return nil
}

func verifyMountTable(mounts []mountEntry, profile runtimeProfile, notes notesProfile) error {
	expected := map[string]mountWant{
		"/": {readOnly: true}, "/dev/null": {},
	}
	for path := range profile.binds {
		expected[path] = mountWant{readOnly: true}
	}
	for path, want := range notes.wants {
		expected[path] = want
	}
	for _, m := range mounts {
		want, ok := expected[m.point]
		if !ok || m.nonPrivate || m.readOnly != want.readOnly || !m.nosuid || m.point != "/dev/null" && !m.nodev {
			return fmt.Errorf("unexpected or unsafe sandbox mount at %s", m.point)
		}
		if want.mask != nil && m.fsType != "tmpfs" {
			return fmt.Errorf("hide mask at %s is not tmpfs", m.point)
		}
		if m.point != "/dev/null" && !ordinaryFilesystem(m.fsType) {
			return fmt.Errorf("unsupported filesystem type %s at %s", m.fsType, m.point)
		}
		delete(expected, m.point)
	}
	for path := range expected {
		return fmt.Errorf("sandbox mount at %s is missing", path)
	}
	return nil
}

func ordinaryFilesystem(kind string) bool {
	switch kind {
	case "ext2", "ext3", "ext4", "btrfs", "xfs", "zfs", "f2fs", "tmpfs", "overlay", "squashfs", "erofs",
		"vfat", "exfat", "ntfs3", "bcachefs", "jfs", "nilfs2", "ubifs":
		return true
	}
	return false
}

func checkNull(path string) error {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(stat.Rdev) != 1 || unix.Minor(stat.Rdev) != 3 {
		return fmt.Errorf("unexpected /dev/null device at %s", path)
	}
	return nil
}
